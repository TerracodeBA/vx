package service

// Organization checks (#47 part 3): [[check]] tables an organization's
// owners and admins define for every repository of the organization. They
// are stored on the server, every save is a new version (the history is
// kept) and goes to the operation log as org.checks. A repository's
// vx.toml adds checks of its own but cannot remove or weaken one of the
// organization's: a vx.toml check with the name of an organization check
// is ignored, with a warning that says so.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// MaxOrgChecksSize limits the [[check]] tables of one organization.
const MaxOrgChecksSize = 64 << 10

// OrgChecks is one version of an organization's checks.
type OrgChecks struct {
	Org     string
	Version int // 0: the organization never defined checks
	Source  string
	Checks  []rules.CheckDef
	By      string // "user:alice"
	At      time.Time
}

// Names are the check names, in the order written.
func (o OrgChecks) Names() []string {
	out := make([]string, len(o.Checks))
	for i, c := range o.Checks {
		out[i] = c.Name
	}
	return out
}

func orgChecksFrom(handle string, v store.OrgCheckVersion) OrgChecks {
	out := OrgChecks{Org: handle, Version: v.Version, Source: v.Source, By: v.CreatedRef, At: v.CreatedAt}
	if len(v.Checks) > 0 {
		_ = json.Unmarshal(v.Checks, &out.Checks)
	}
	for i := range out.Checks {
		out.Checks[i].Origin = rules.OriginOrg
	}
	return out
}

// ParseOrgChecks reads an organization's [[check]] tables: the same
// fields and validation as in vx.toml, and after may only name checks of
// the organization (a repository's checks are not known to it).
func ParseOrgChecks(source string) ([]rules.CheckDef, error) {
	if len(source) > MaxOrgChecksSize {
		return nil, errf(CodeInvalid, "the organization checks are %d bytes; at most %d", len(source), MaxOrgChecksSize)
	}
	defs, err := rules.ParseChecks([]byte(source))
	if err != nil {
		return nil, errf(CodeInvalid, "organization checks: %s", strings.ReplaceAll(err.Error(), rules.File+" ", ""))
	}
	if probs := rules.ValidatePipeline(rules.StepsOf(defs), nil); len(probs) > 0 {
		msgs := make([]string, len(probs))
		for i, p := range probs {
			msgs[i] = p.String()
		}
		return nil, errf(CodeInvalid, "organization checks: %s", strings.Join(msgs, "; "))
	}
	for i := range defs {
		defs[i].Origin = rules.OriginOrg
	}
	return defs, nil
}

// GetOrgChecks returns the checks in force for an organization (members
// and superadmins).
func (s *Service) GetOrgChecks(ctx context.Context, actor Actor, org string) (OrgChecks, error) {
	a, err := s.loadOrg(ctx, nil, actor, org)
	if err != nil {
		return OrgChecks{}, err
	}
	v, ok, err := s.Store.OrgChecks(ctx, a.org.ID)
	if err != nil || !ok {
		return OrgChecks{Org: a.org.Handle}, err
	}
	return orgChecksFrom(a.org.Handle, v), nil
}

// OrgCheckHistory lists every saved version of an organization's checks,
// newest first (members and superadmins).
func (s *Service) OrgCheckHistory(ctx context.Context, actor Actor, org string) ([]OrgChecks, error) {
	a, err := s.loadOrg(ctx, nil, actor, org)
	if err != nil {
		return nil, err
	}
	vs, err := s.Store.OrgCheckHistory(ctx, a.org.ID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]OrgChecks, len(vs))
	for i, v := range vs {
		out[i] = orgChecksFrom(a.org.Handle, v)
	}
	return out, nil
}

// orgChecksChange is the operation log entry of a save.
type orgChecksChange struct {
	Version int      `json:"version"`
	Checks  []string `json:"checks"`
	Source  string   `json:"source,omitempty"`
}

// SetOrgChecks saves a new version of an organization's checks (owners
// and admins, people only: an agent changes a repository's checks by
// landing vx.toml, never the organization's). An empty source removes
// every organization check; that is a version too. Saving the same source
// again changes nothing and returns the version in force, with changed
// false.
func (s *Service) SetOrgChecks(ctx context.Context, actor Actor, org, source string) (out OrgChecks, changed bool, err error) {
	if err := peopleOnly(actor, "change the organization checks"); err != nil {
		return OrgChecks{}, false, err
	}
	defs, err := ParseOrgChecks(source)
	if err != nil {
		return OrgChecks{}, false, err
	}
	data, err := json.Marshal(defs)
	if err != nil {
		return OrgChecks{}, false, err
	}
	if defs == nil {
		data = []byte("[]")
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.manageable(ctx, tx, actor, org, "changing the organization checks")
		if err != nil {
			return err
		}
		prev, had, err := tx.OrgChecks(ctx, a.org.ID)
		if err != nil {
			return err
		}
		if had && prev.Source == source {
			out = orgChecksFrom(a.org.Handle, prev)
			return nil
		}
		h := actor.Human()
		v, err := tx.AddOrgCheckVersion(ctx, store.OrgCheckVersion{OrgID: a.org.ID, Source: source, Checks: data, CreatedBy: &h.ID, CreatedRef: h.Ref()})
		if err != nil {
			return err
		}
		out, changed = orgChecksFrom(a.org.Handle, v), true
		var before any
		if had {
			p := orgChecksFrom(a.org.Handle, prev)
			before = orgChecksChange{Version: p.Version, Checks: p.Names(), Source: p.Source}
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.checks", a.org.Handle, before, orgChecksChange{Version: out.Version, Checks: out.Names(), Source: source})
	})
	return out, changed, classify(err)
}

// orgChecksOf returns the checks in force for a repository's organization,
// without a permission check: callers have authorized the repository.
func (s *Service) orgChecksOf(ctx context.Context, repo store.Repo) (OrgChecks, error) {
	v, ok, err := s.Store.OrgChecks(ctx, repo.OrgID)
	if err != nil || !ok {
		return OrgChecks{Org: repo.Org}, err
	}
	return orgChecksFrom(repo.Org, v), nil
}

// MergeOrgChecks puts a repository's checks together: the organization's
// first, then those of vx.toml. A vx.toml check with the name of an
// organization check is left out, with a warning: a repository cannot
// remove or weaken an organization check, only add its own. A vx.toml
// check that runs after a name that is neither is reported too.
func MergeOrgChecks(org OrgChecks, repo []rules.CheckDef) ([]rules.CheckDef, []string) {
	out := append([]rules.CheckDef(nil), org.Checks...)
	byName := map[string]bool{}
	for _, c := range org.Checks {
		byName[c.Name] = true
	}
	var warnings []string
	for _, c := range repo {
		if byName[c.Name] {
			warnings = append(warnings, fmt.Sprintf("%s [[check]] %q is ignored: organization %s defines a check of that name, and a repository cannot change or remove an organization check. Rename yours to add a check of your own", rules.File, c.Name, org.Org))
			continue
		}
		out = append(out, c)
	}
	known := func(n string) bool { return byName[n] }
	var repoSteps []rules.Step
	for _, c := range out[len(org.Checks):] {
		repoSteps = append(repoSteps, rules.Step{Name: c.Name, After: c.After, Inputs: c.Inputs})
	}
	for _, p := range rules.ValidatePipeline(repoSteps, known) {
		warnings = append(warnings, rules.File+" "+p.String())
	}
	return out, warnings
}

// CheckOrigin says where a check comes from, for vx rules, vx check status
// and the web: "organization acme (version 3)", "vx.toml line 12",
// "vx.toml [checks] required" or "vx.toml [[policy]]".
func CheckOrigin(c rules.CheckDef, org OrgChecks) string {
	switch c.Origin {
	case rules.OriginOrg:
		if org.Version > 0 {
			return fmt.Sprintf("organization %s (version %d)", org.Org, org.Version)
		}
		return "organization " + org.Org
	case rules.OriginRequired:
		return rules.File + " [checks] required"
	case rules.OriginPolicy:
		return rules.File + " [[policy]]"
	}
	if c.Line > 0 {
		return fmt.Sprintf("%s line %d", rules.File, c.Line)
	}
	return rules.File
}

// orgCheckWarnings warn, on the land that changes vx.toml, about its
// checks that an organization check overrides or that run after a name
// that is no check.
func (s *Service) orgCheckWarnings(ctx context.Context, repo store.Repo, base, tree hash.Hash) []engine.Warning {
	if !s.rulesFileChanged(ctx, base, tree) {
		return nil
	}
	r, err := rules.At(ctx, s.Blobs, tree)
	if err != nil {
		return nil // ruleWarnings reports a vx.toml that does not parse
	}
	org, err := s.orgChecksOf(ctx, repo)
	if err != nil {
		return nil
	}
	_, msgs := MergeOrgChecks(org, r.Checks)
	out := make([]engine.Warning, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, engine.Warning{Path: rules.File, Kind: rules.WarningRule, Name: rules.File, Detail: m})
	}
	return out
}

// RepoOrgChecks returns the organization checks that apply to a repository
// (read on the repository: its readers see what it is held to without
// being members of the organization).
func (s *Service) RepoOrgChecks(ctx context.Context, actor Actor, repoName string) (OrgChecks, error) {
	repo, _, err := s.Authorize(ctx, actor, repoName, RoleRead)
	if err != nil {
		return OrgChecks{}, err
	}
	return s.orgChecksOf(ctx, repo)
}
