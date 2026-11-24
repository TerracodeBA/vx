package service

// Merging duplicate accounts (superadmins): one person signed up twice,
// say once with a token and once with GitHub. Everything that lets the
// merged account act moves to the kept one; the merged account stays,
// disabled, so changes, notes and the op log that name it still resolve.

import (
	"context"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// MergeResult is what MergePeople did.
type MergeResult struct {
	Kept, Merged store.Principal
	Moved        store.MergeStats
}

// MergePeople merges the person merged into keep (superadmins only):
// tokens, agents, organization and team memberships, repository grants,
// single sign-on identities and (when keep has none) the GitHub link move
// to keep, the higher role winning where both had one. Authorship stays:
// changes keep naming the merged person and their agents, whose handles do
// not change. The merged person is disabled and the merge recorded in the
// operation log (person.merge).
//
// A superadmin never merges their own account, in either direction:
// through the admin path that would hand them a customer's repository
// roles, or hand theirs to a customer.
func (s *Service) MergePeople(ctx context.Context, actor Actor, keepRef, mergedRef string) (MergeResult, error) {
	if err := peopleOnly(actor, "merge accounts"); err != nil {
		return MergeResult{}, err
	}
	if !s.IsSuperadmin(ctx, actor) {
		return MergeResult{}, errf(CodePermission, "only superadmins merge accounts; %s is not one", actor.Ref())
	}
	var res MergeResult
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		keep, err := s.mergeable(ctx, tx, keepRef, "keep")
		if err != nil {
			return err
		}
		merged, err := s.mergeable(ctx, tx, mergedRef, "merge")
		if err != nil {
			return err
		}
		if keep.ID == merged.ID {
			return errf(CodeInvalid, "%s and %s are the same person; choose two different accounts", keepRef, mergedRef)
		}
		if keep.ID == actor.ID || merged.ID == actor.ID {
			return errf(CodePermission, "superadmins cannot merge their own account (%s) with someone else's through the admin page: it would move repository roles between you and a customer. Ask another superadmin, or sign in to both accounts yourself and connect them", actor.Ref())
		}
		moved, err := tx.MergePeople(ctx, keep.ID, merged.ID)
		if err != nil {
			return err
		}
		res = MergeResult{Kept: keep, Merged: merged, Moved: moved}
		return tx.LogOp(ctx, nil, &actor.ID, "person.merge", merged.Ref(),
			map[string]any{"handle": merged.Handle, "email": merged.Email, "site_admin": merged.SiteAdmin},
			map[string]any{"into": keep.Ref(), "tokens": moved.Tokens, "agents": moved.Agents, "orgs": moved.Orgs, "teams": moved.Teams,
				"grants": moved.Grants, "identities": moved.Identities, "github": moved.GitHub, "disabled": true})
	})
	return res, classify(err)
}

func (s *Service) mergeable(ctx context.Context, tx *store.Tx, ref, which string) (store.Principal, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.Principal{}, errf(CodeInvalid, "which account to %s? Give a handle like alice", which)
	}
	p, err := s.resolvePerson(ctx, tx, ref)
	if err != nil {
		return store.Principal{}, err
	}
	if disabled, _, err := tx.PrincipalDisabled(ctx, p.ID); err != nil {
		return store.Principal{}, err
	} else if disabled {
		return store.Principal{}, errf(CodeFailedPrecondition, "%s was already merged into another account and is disabled", p.Ref())
	}
	return p, nil
}
