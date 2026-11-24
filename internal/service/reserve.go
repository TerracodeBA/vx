package service

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// GetRules returns the vx.toml rules of a repository (read): at the head of
// effort when one is given, else at the head of the default branch. No
// vx.toml: empty Rules. A vx.toml that does not parse is an error that
// says where.
func (s *Service) GetRules(ctx context.Context, actor Actor, repoName, effort string) (Rules, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return Rules{}, err
	}
	var at hash.Hash
	where := ""
	if effort != "" {
		eff, err := s.Store.GetEffort(ctx, repo.ID, effort)
		if err != nil {
			return Rules{}, classify(err)
		}
		at, where = eff.Head, "effort "+eff.Slug
	} else {
		b, err := s.Store.GetBranch(ctx, repo.ID, repo.DefaultBranch)
		if errors.Is(err, store.ErrNotFound) {
			return Rules{}, nil // nothing imported yet: no rules
		}
		if err != nil {
			return Rules{}, err
		}
		at, where = b.Head, repo.DefaultBranch
	}
	cs, err := s.Store.GetChangeset(ctx, repo.ID, at)
	if err != nil {
		return Rules{}, classify(err)
	}
	r, err := rules.At(ctx, s.Blobs, cs.Tree)
	if err != nil {
		var re *rules.Error
		if errors.As(err, &re) {
			return Rules{}, errf(CodeFailedPrecondition, "%s at %s %s does not parse:\n%v", RulesFile, where, at.Short(), err)
		}
		return Rules{}, err
	}
	if r.Source != "" {
		r.Source = fmt.Sprintf("%s at %s %s", RulesFile, where, at.Short())
	}
	return r, nil
}

// Reservation is a value vx reserve handed out.
type Reservation = store.Reservation

// ListReservations lists the values vx reserve handed out in a repository
// (read), newest first; rule narrows it to one [[unique]] rule.
func (s *Service) ListReservations(ctx context.Context, actor Actor, repoName, rule string) ([]Reservation, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return nil, err
	}
	return s.Store.ListReservations(ctx, repo.ID, rule)
}

// reserveAttempts bounds how often Reserve retries when concurrent calls
// take the value it computed.
const reserveAttempts = 20

// Reserve hands out the next free value of a unique rule (write): one more
// than the largest key used in the head of the effort's target branch, in
// the head of any open effort of the repository, or reserved before,
// formatted like the existing keys (same zero padding). The value is
// recorded in rule_reservations, whose primary key makes concurrent calls
// get different values: the loser computes again. example is a file name
// to use, like "internal/store/migrations/017_<name>.sql".
func (s *Service) Reserve(ctx context.Context, actor Actor, repoName, effort, rule string) (value, example string, err error) {
	v, err := s.effortFor(ctx, actor, repoName, effort, RoleWrite)
	if err != nil {
		return "", "", err
	}
	if v.Effort.State != store.EffortOpen {
		return "", "", errf(CodeFailedPrecondition, "effort %s is %s; reserve values for an open effort", v.Effort.Slug, v.Effort.State)
	}
	head, err := s.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Head)
	if err != nil {
		return "", "", err
	}
	r, err := rules.At(ctx, s.Blobs, head.Tree)
	if err != nil {
		return "", "", errf(CodeFailedPrecondition, "%s in effort %s does not parse, fix it first:\n%v", RulesFile, v.Effort.Slug, err)
	}
	u, ok := r.UniqueRule(rule)
	if !ok {
		var names []string
		for _, x := range r.Unique {
			names = append(names, x.Name)
		}
		if len(names) == 0 {
			return "", "", errf(CodeFailedPrecondition, "%s in effort %s has no [[unique]] rules; add one (see vx rules)", RulesFile, v.Effort.Slug)
		}
		return "", "", errf(CodeFailedPrecondition, "%s has no [[unique]] rule %q; its rules: %s", RulesFile, rule, strings.Join(names, ", "))
	}
	g, err := rules.CompileGlob(u.Glob)
	if err != nil {
		return "", "", errf(CodeFailedPrecondition, "rule %s: %v", u.Name, err)
	}
	key := regexp.MustCompile(u.Key) // Parse checked it

	// Keys in use: the target branch, every open effort, earlier reservations.
	trees := []hash.Hash{head.Tree}
	if b, err := s.Store.GetBranch(ctx, v.Repo.ID, v.Effort.BaseBranch); err == nil {
		if cs, err := s.Store.GetChangeset(ctx, v.Repo.ID, b.Head); err == nil {
			trees = append(trees, cs.Tree)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", "", err
	}
	open, err := s.Store.ListEfforts(ctx, v.Repo.ID, store.EffortOpen)
	if err != nil {
		return "", "", err
	}
	for _, e := range open {
		if e.ID == v.Effort.ID {
			continue
		}
		cs, err := s.Store.GetChangeset(ctx, v.Repo.ID, e.Head)
		if err != nil {
			return "", "", err
		}
		trees = append(trees, cs.Tree)
	}
	used := keySet{}
	seen := map[hash.Hash]bool{}
	for _, t := range trees {
		if seen[t] {
			continue
		}
		seen[t] = true
		files, err := rules.Files(ctx, s.Blobs, t, g)
		if err != nil {
			return "", "", err
		}
		for _, p := range files {
			if k, ok := rules.KeyOf(key, p); ok {
				used.add(k, p)
			}
		}
	}

	for attempt := 0; attempt < reserveAttempts; attempt++ {
		res, err := s.Store.ListReservations(ctx, v.Repo.ID, u.Name)
		if err != nil {
			return "", "", err
		}
		all := used.clone()
		for _, x := range res {
			all.add(x.Value, "")
		}
		next, err := all.next()
		if err != nil {
			return "", "", errf(CodeFailedPrecondition, "rule %s: %v", u.Name, err)
		}
		ok, err := s.Store.InsertReservation(ctx, v.Repo.ID, u.Name, next, &v.Effort.ID, &actor.ID)
		if err != nil {
			return "", "", err
		}
		if ok {
			_ = s.Store.LogOp(ctx, &v.Repo.ID, &actor.ID, "reserve", v.Effort.Slug, nil, map[string]string{"rule": u.Name, "value": next})
			return next, exampleName(key, g, all.sample, next), nil
		}
	}
	return "", "", errf(CodeUnavailable, "rule %s is too busy: gave up after %d attempts, try again", u.Name, reserveAttempts)
}

// keySet collects the keys of a unique rule.
type keySet struct {
	keys   map[string]bool
	sample string // a file path with the largest numeric key, for the example
	max    *big.Int
}

func (k *keySet) add(key, path string) {
	if k.keys == nil {
		k.keys = map[string]bool{}
	}
	k.keys[key] = true
	n, ok := new(big.Int).SetString(key, 10)
	if !ok || n.Sign() < 0 {
		return
	}
	switch {
	case k.max == nil || n.Cmp(k.max) > 0:
		k.max, k.sample = n, path
	case n.Cmp(k.max) == 0 && k.sample == "":
		k.sample = path
	}
}

func (k keySet) clone() keySet {
	out := keySet{keys: map[string]bool{}, sample: k.sample}
	for x := range k.keys {
		out.keys[x] = true
	}
	if k.max != nil {
		out.max = new(big.Int).Set(k.max)
	}
	return out
}

// next is one more than the largest numeric key, padded with zeros to the
// width of the widest key that has leading zeros ("016" -> "017"). With no
// keys yet it is "1".
func (k keySet) next() (string, error) {
	if k.max == nil {
		if len(k.keys) > 0 {
			var some []string
			for x := range k.keys {
				some = append(some, x)
			}
			sort.Strings(some)
			return "", fmt.Errorf("its keys are not numbers (for example %q); vx reserve hands out numbers only", some[0])
		}
		return "1", nil
	}
	width := 0
	for x := range k.keys {
		if _, ok := new(big.Int).SetString(x, 10); ok && len(x) > width && strings.HasPrefix(x, "0") {
			width = len(x)
		}
	}
	n := new(big.Int).Add(k.max, big.NewInt(1)).String()
	if len(n) < width {
		n = strings.Repeat("0", width-len(n)) + n
	}
	return n, nil
}

// exampleName is the name a file using value would have: the sample file
// with its key replaced and the rest of its name after the key's match
// replaced by "<name>", e.g. "internal/store/migrations/017_<name>.sql".
func exampleName(key *regexp.Regexp, g rules.Glob, sample, value string) string {
	dir, name, ext := g.Dir(), "", ""
	if sample != "" {
		if i := strings.LastIndexByte(sample, '/'); i >= 0 {
			dir, name = sample[:i], sample[i+1:]
		} else {
			dir, name = "", sample
		}
	}
	join := func(file string) string {
		if dir == "" {
			return file
		}
		return dir + "/" + file
	}
	if m := key.FindStringSubmatchIndex(name); m != nil && name != "" {
		rest := name[m[1]:]
		if i := strings.LastIndexByte(rest, '.'); i >= 0 {
			ext = rest[i:]
		}
		return join(name[:m[2]] + value + name[m[3]:m[1]] + "<name>" + ext)
	}
	last := g.String()[strings.LastIndexByte(g.String(), '/')+1:]
	if i := strings.LastIndexByte(last, '.'); i >= 0 && !strings.ContainsAny(last[i:], "*?") {
		ext = last[i:]
	}
	return join(value + "_<name>" + ext)
}
