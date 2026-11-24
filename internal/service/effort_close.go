package service

import (
	"context"
	"log/slog"
	"regexp"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/store"
)

// CloseEffort marks an effort merged or abandoned (write). Its changes and
// board stay readable; it can no longer be landed into, and lists, context
// and overlap forecasts leave it out. Closing a closed effort again with the
// same state changes nothing.
func (s *Service) CloseEffort(ctx context.Context, actor Actor, repoName, slug string, state store.EffortState) (store.Effort, error) {
	if state != store.EffortMerged && state != store.EffortAbandoned {
		return store.Effort{}, errf(CodeInvalid, "an effort closes as merged or abandoned, not %q", state)
	}
	v, err := s.effortFor(ctx, actor, repoName, slug, RoleWrite)
	if err != nil {
		return store.Effort{}, err
	}
	switch v.Effort.State {
	case state:
		return v.Effort, nil
	case store.EffortOpen, store.EffortShipping:
	default:
		return store.Effort{}, errf(CodeFailedPrecondition, "effort %s is already %s", slug, v.Effort.State)
	}
	if err := s.closeEffort(ctx, &actor, v.Repo, v.Effort, state, "by hand"); err != nil {
		return store.Effort{}, err
	}
	return s.Store.GetEffort(ctx, v.Repo.ID, slug)
}

func (s *Service) closeEffort(ctx context.Context, actor *Actor, repo store.Repo, e store.Effort, state store.EffortState, how string) error {
	ship := "merged"
	if state == store.EffortAbandoned {
		ship = "closed"
	}
	var who *uuid.UUID
	if actor != nil {
		id := actor.ID
		who = &id
	}
	return classify(s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetEffortState(ctx, e.ID, state); err != nil {
			return err
		}
		if err := tx.CloseShipments(ctx, e.ID, ship); err != nil {
			return err
		}
		return tx.LogOp(ctx, &repo.ID, who, "effort.close", e.Slug, map[string]any{"state": e.State}, map[string]any{"state": state, "how": how})
	}))
}

// changeIDTrailer is the trailer vx writes into every shipped commit. It
// survives rebase and merge commits on the git host; a squash merge loses
// it, which the pull request webhook covers.
var changeIDTrailer = regexp.MustCompile(`(?m)^Vx-Change-Id:\s*(\S+)\s*$`)

// markMergedEfforts marks the open efforts on branch merged once the branch
// contains every change they landed. It runs after commits of the branch
// were imported (vx repo sync, a push webhook, a merged pull request). It
// returns how many efforts it marked; errors for one effort are logged and
// do not stop the others.
func (s *Service) markMergedEfforts(ctx context.Context, repo store.Repo, branch string) int {
	s.markMergedShipments(ctx, repo, branch)
	b, err := s.Store.GetBranch(ctx, repo.ID, branch)
	if err != nil {
		return 0
	}
	effs, err := s.Store.ListEfforts(ctx, repo.ID, store.EffortOpen)
	if err != nil {
		slog.Default().Warn("efforts merged check", "repo", repo.Name, "err", err)
		return 0
	}
	marked := 0
	for _, e := range effs {
		if e.BaseBranch != branch || e.Head == e.Base {
			continue
		}
		mine, err := s.Store.Chain(ctx, repo.ID, e.Head, e.Base, 5000)
		if err != nil || len(mine) == 0 {
			continue
		}
		upstream, err := s.Store.Chain(ctx, repo.ID, b.Head, e.Base, 5000)
		if err != nil {
			continue
		}
		onBranch := map[string]bool{}
		for _, c := range upstream {
			for _, m := range changeIDTrailer.FindAllStringSubmatch(c.Intent, -1) {
				onBranch[m[1]] = true
			}
		}
		all := true
		for _, c := range mine {
			if c.Origin == object.OriginVX && !onBranch[c.ChangeID] {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		if err := s.closeEffort(ctx, nil, repo, e, store.EffortMerged, "its changes are on "+branch); err != nil {
			slog.Default().Warn("mark effort merged", "repo", repo.Name, "effort", e.Slug, "err", err)
			continue
		}
		marked++
	}
	return marked
}
