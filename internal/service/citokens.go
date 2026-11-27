package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// CI tokens (#47). A repository admin creates a CI token for a CI system:
// a service principal of its own with one token, which may only report
// check results on that repository (for the named checks, or every check)
// and read the inputs of its jobs, including the files they name through
// the object API. It has no repository grant, so every other operation,
// on this repository or any other, reports "not found" to it, exactly as
// to a stranger. Revoking it revokes its token at once.

// ciNamePattern is what a CI token's name and a check name look like.
var ciNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// MaxCITokenChecks bounds the checks one CI token may name.
const MaxCITokenChecks = 50

// CITokenView is a CI token without its secret.
type CITokenView struct {
	store.CIToken
	CreatedByRef string
}

// Ref is the reference the token's results are recorded under.
func (v CITokenView) Ref() string { return "service:" + v.Handle }

// MayReport reports whether the token may report results of check.
func (v CITokenView) MayReport(check string) bool {
	return len(v.Checks) == 0 || slices.Contains(v.Checks, check)
}

func cleanCheckNames(flag string, names []string) ([]string, error) {
	out := []string{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(out, n) {
			continue
		}
		if !ciNamePattern.MatchString(n) {
			return nil, errf(CodeInvalid, "%s %q: a check name is lowercase letters, digits, dot, dash or underscore", flag, n)
		}
		out = append(out, n)
	}
	if len(out) > MaxCITokenChecks {
		return nil, errf(CodeInvalid, "%s names %d checks; keep it to %d", flag, len(out), MaxCITokenChecks)
	}
	sort.Strings(out)
	return out, nil
}

// CreateCIToken creates a CI token on a repository and returns it with its
// secret, which is shown once. Only a person with admin on the repository
// may; agents never mint tokens.
func (s *Service) CreateCIToken(ctx context.Context, actor Actor, repo, name string, checks []string) (CITokenView, string, error) {
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return CITokenView{}, "", err
	}
	if actor.Kind != store.KindHuman {
		return CITokenView{}, "", errf(CodePermission, "only people create CI tokens; %s is not a person. Ask %s to run `vx repo ci-token create %s --repo %s`", actor.Ref(), ownerRef(actor), name, repo)
	}
	name = strings.TrimSpace(name)
	if !ciNamePattern.MatchString(name) {
		return CITokenView{}, "", errf(CodeInvalid, "CI token name %q: use lowercase letters, digits, dot, dash or underscore, like github-actions", name)
	}
	if checks, err = cleanCheckNames("--checks", checks); err != nil {
		return CITokenView{}, "", err
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return CITokenView{}, "", err
	}
	var out store.CIToken
	var secret string
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		p, err := tx.CreatePrincipal(ctx, store.Principal{Kind: store.KindService, Handle: "ci-" + hex.EncodeToString(raw[:]),
			DisplayName: "CI token " + name + " of " + r.Name})
		if err != nil {
			return err
		}
		tok, sec, err := tx.IssueToken(ctx, p.ID, "CI token "+name+" of "+r.Name)
		if err != nil {
			return err
		}
		secret = sec
		if out, err = tx.InsertCIToken(ctx, store.CIToken{RepoID: r.ID, Name: name, PrincipalID: p.ID, TokenID: tok.ID,
			Checks: checks, CreatedBy: &actor.ID, CreatedAt: s.Clock()}); err != nil {
			return err
		}
		return tx.LogOp(ctx, &r.ID, &actor.ID, "repo.ci_token.create", r.Name, nil,
			map[string]any{"name": name, "checks": checks, "principal": p.Ref()})
	})
	if errors.Is(err, store.ErrExists) {
		return CITokenView{}, "", errf(CodeExists, "%s already has a CI token named %s; revoke it first (vx repo ci-token revoke %s --repo %s) or pick another name", r.Name, name, name, r.Name)
	}
	if err != nil {
		return CITokenView{}, "", classify(err)
	}
	return CITokenView{CIToken: out, CreatedByRef: actor.Ref()}, secret, nil
}

// ListCITokens lists a repository's CI tokens (revoked ones too with
// withRevoked). Repository admins only.
func (s *Service) ListCITokens(ctx context.Context, actor Actor, repo string, withRevoked bool) ([]CITokenView, error) {
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.ListCITokens(ctx, r.ID, withRevoked)
	if err != nil {
		return nil, err
	}
	out := make([]CITokenView, 0, len(rows))
	for _, t := range rows {
		v := CITokenView{CIToken: t}
		if t.CreatedBy != nil {
			v.CreatedByRef = s.principalRef(ctx, *t.CreatedBy)
		}
		out = append(out, v)
	}
	return out, nil
}

// RevokeCIToken revokes a repository's CI token by name, with its token.
// Repository admins only (people and their agents).
func (s *Service) RevokeCIToken(ctx context.Context, actor Actor, repo, name string) (CITokenView, error) {
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return CITokenView{}, err
	}
	var out store.CIToken
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if out, err = tx.RevokeCIToken(ctx, r.ID, strings.TrimSpace(name), s.Clock()); err != nil {
			return err
		}
		return tx.LogOp(ctx, &r.ID, &actor.ID, "repo.ci_token.revoke", r.Name, map[string]any{"name": out.Name, "checks": out.Checks}, nil)
	})
	if errors.Is(err, store.ErrNotFound) {
		return CITokenView{}, errf(CodeNotFound, "%s has no active CI token named %s; vx repo ci-token list --repo %s shows them", r.Name, name, r.Name)
	}
	if err != nil {
		return CITokenView{}, classify(err)
	}
	return CITokenView{CIToken: out}, nil
}

// ciTokenOf returns the CI token actor authenticates as, or nil when actor
// is not a CI token.
func (s *Service) ciTokenOf(ctx context.Context, actor Actor) (*CITokenView, error) {
	if actor.Kind != store.KindService {
		return nil, nil
	}
	t, err := s.Store.CITokenOf(ctx, actor.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &CITokenView{CIToken: t}, nil
}

// ciRepo returns the repository named repo when ci is a token of it, and
// "not found" otherwise, exactly like a repository the caller cannot read.
func (s *Service) ciRepo(ctx context.Context, ci *CITokenView, repo string) (store.Repo, error) {
	if ci == nil || ci.RepoName != repo {
		return store.Repo{}, repoNotFound(repo)
	}
	r, _, err := s.Store.RepoRoleFacts(ctx, repo, ci.PrincipalID, true, s.Clock())
	if err != nil {
		return store.Repo{}, repoNotFound(repo)
	}
	return r, nil
}

// authorizeReport is the access check for reporting a check result: people
// and agents need write on the repository; a CI token reports only on its
// repository and only the checks it names. It returns the CI token's id
// for the result, nil for everyone else.
func (s *Service) authorizeReport(ctx context.Context, actor Actor, repo, check string) (store.Repo, *uuid.UUID, error) {
	ci, err := s.ciTokenOf(ctx, actor)
	if err != nil {
		return store.Repo{}, nil, err
	}
	if ci == nil {
		r, _, err := s.Authorize(ctx, actor, repo, RoleWrite)
		return r, nil, err
	}
	r, err := s.ciRepo(ctx, ci, repo)
	if err != nil {
		return store.Repo{}, nil, err
	}
	if !ci.MayReport(check) {
		return store.Repo{}, nil, errf(CodePermission, "CI token %s of %s may report only %s, not %s; a repository admin can add it: vx repo ci-token create NAME --repo %s --checks %s",
			ci.Name, r.Name, strings.Join(ci.Checks, ", "), check, r.Name, strings.Join(append(slices.Clone(ci.Checks), check), ","))
	}
	id := ci.ID
	return r, &id, nil
}

// authorizeCheckRead is the access check for reading check results, jobs
// and their inputs: read on the repository, or a CI token of it.
func (s *Service) authorizeCheckRead(ctx context.Context, actor Actor, repo string) (store.Repo, error) {
	ci, err := s.ciTokenOf(ctx, actor)
	if err != nil {
		return store.Repo{}, err
	}
	if ci == nil {
		r, _, err := s.Authorize(ctx, actor, repo, RoleRead)
		return r, err
	}
	return s.ciRepo(ctx, ci, repo)
}
