package service

// Automatic rebases (v0.4, #7): when a branch moves (vx repo sync, a push
// or merge webhook of any host), each open effort on it is rebased onto
// the new head as the vx principal when its changes replay cleanly, or
// told which files conflict. vx.toml [automation] rebase on the branch
// chooses: auto (the default), notify or off.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// AutoRebase is what an automatic rebase did to one effort.
type AutoRebase struct {
	Effort    string   // slug
	Rebased   bool     // the effort now sits on the branch head
	Conflicts []string // files that conflict; the effort was left alone
	Behind    bool     // notify mode: the effort is behind and replays cleanly
}

// BranchMoved runs after vx imported branch of repo: it marks the efforts
// whose changes reached the branch merged, then rebases the open efforts
// on it, or tells them they need a rebase (vx.toml [automation] rebase).
// by names what moved the branch, for the op log ("github push"). Errors
// are logged: the import itself succeeded.
func (s *Service) BranchMoved(ctx context.Context, repo store.Repo, branch, by string) []AutoRebase {
	s.markMergedEfforts(ctx, repo, branch)
	return s.autoRebase(ctx, repo, branch, by)
}

// autoRebase rebases or notifies the open efforts on branch.
func (s *Service) autoRebase(ctx context.Context, repo store.Repo, branch, by string) []AutoRebase {
	log := slog.Default().With("repo", repo.Name, "branch", branch)
	b, err := s.Store.GetBranch(ctx, repo.ID, branch)
	if err != nil {
		return nil
	}
	head, err := s.Store.GetChangeset(ctx, repo.ID, b.Head)
	if err != nil {
		log.Warn("automatic rebase: branch head", "err", err)
		return nil
	}
	mode := rules.RebaseAuto
	if r, err := s.RulesAt(ctx, head.Tree); err == nil {
		mode = r.RebaseMode()
	} else {
		log.Warn("automatic rebase: vx.toml does not parse, rebasing by default", "err", err)
	}
	if mode == rules.RebaseOff {
		return nil
	}
	effs, err := s.Store.ListEfforts(ctx, repo.ID, store.EffortOpen)
	if err != nil {
		log.Warn("automatic rebase: efforts", "err", err)
		return nil
	}
	vx, err := s.systemPrincipal(ctx)
	if err != nil {
		log.Warn("automatic rebase", "err", err)
		return nil
	}
	actor := Actor{Principal: vx}
	var out []AutoRebase
	for _, e := range effs {
		if e.BaseBranch != branch || e.Base == b.Head {
			continue
		}
		if _, err := s.Store.ActiveRebase(ctx, e.ID); err == nil {
			continue // someone is rebasing it by hand
		} else if !errors.Is(err, store.ErrNotFound) {
			log.Warn("automatic rebase: active rebase", "effort", e.Slug, "err", err)
			continue
		}
		res, err := s.autoRebaseOne(ctx, actor, repo, e, b, head, mode, by)
		if err != nil {
			log.Warn("automatic rebase", "effort", e.Slug, "err", err)
			continue
		}
		out = append(out, res)
	}
	return out
}

func (s *Service) autoRebaseOne(ctx context.Context, actor Actor, repo store.Repo, eff store.Effort, b store.Branch, head store.ChangesetRow, mode, by string) (AutoRebase, error) {
	res := AutoRebase{Effort: eff.Slug}
	chain, err := s.Store.Chain(ctx, repo.ID, eff.Head, eff.Base, 10000)
	if err != nil {
		return res, err
	}
	dry, err := s.replayChain(ctx, repo.ID, chain, head.Tree, b.Name, replayForecast)
	if err != nil {
		return res, err
	}
	onto := b.Name + " at " + ontoSHA(head)
	changes := len(dry.Steps)
	if dry.ConflictedAt != nil || len(dry.Conflicts) > 0 {
		res.Conflicts = conflictPaths(dry.Conflicts)
		return res, s.rebaseNote(ctx, actor, eff, "conflict:"+strings.Join(res.Conflicts, ","), res.Conflicts,
			fmt.Sprintf("This effort needs a rebase: %s moved to %s, and replaying its changes conflicts in %s. Rebase it and resolve them: vx effort rebase (vx did not rebase it).",
				b.Name, ontoSHA(head), fileList(res.Conflicts)), by)
	}
	if mode == rules.RebaseNotify {
		res.Behind = true
		return res, s.rebaseNote(ctx, actor, eff, "behind", nil,
			fmt.Sprintf("%s moved to %s; this effort replays cleanly onto it. Rebase when it suits you: vx effort rebase.", b.Name, ontoSHA(head)), by)
	}
	v, err := s.rebaseOnto(ctx, actor, repo, eff, b)
	if err != nil {
		return res, err
	}
	if v.Rebase.State == store.RebaseStopped {
		// The forecast said clean; a real conflict means the merge rules
		// differ. Leave the effort as it was and say so.
		if err := s.abortRebaseAs(ctx, actor, repo, eff, v.Rebase); err != nil {
			return res, err
		}
		for _, c := range v.Rebase.Conflicts {
			res.Conflicts = append(res.Conflicts, c.Path)
		}
		res.Conflicts = uniqueSorted(res.Conflicts)
		return res, s.rebaseNote(ctx, actor, eff, "conflict:"+strings.Join(res.Conflicts, ","), res.Conflicts,
			fmt.Sprintf("This effort needs a rebase: %s moved to %s, and replaying its changes conflicts in %s. Rebase it and resolve them: vx effort rebase (vx did not rebase it).",
				b.Name, ontoSHA(head), fileList(res.Conflicts)), by)
	}
	res.Rebased = !v.UpToDate
	if !res.Rebased || changes == 0 {
		return res, nil // nothing of this effort moved: no news
	}
	return res, s.rebaseNote(ctx, actor, eff, "rebased:"+head.Hash.String(), nil,
		fmt.Sprintf("Rebased automatically onto %s (%d change(s) replayed cleanly); evidence predates the rebase, so run the checks again before you ship. Open workspaces keep working: vx sync brings the rebased files.", onto, changes), by)
}

// rebaseNote posts the effort's rebase note, replacing the previous one.
func (s *Service) rebaseNote(ctx context.Context, actor Actor, eff store.Effort, digest string, targets []string, body, by string) error {
	if len(targets) > MaxNoteTargets {
		targets = targets[:MaxNoteTargets]
	}
	_, err := s.postSystemNote(ctx, actor.Principal, eff, systemNote{
		Key: "rebase", Digest: digest, Kind: NoteRebase, Targets: targets, Body: finishNote(body),
		Op: "note.rebase", Extra: map[string]any{"by": by, "conflicts": targets},
	})
	return err
}

// abortRebaseAs drops a stopped rebase vx started.
func (s *Service) abortRebaseAs(ctx context.Context, actor Actor, repo store.Repo, eff store.Effort, r store.Rebase) error {
	seen := r.Version
	r.State, r.UpdatedBy = store.RebaseAborted, actor.ID
	return classify(s.Store.InTx(ctx, func(tx *store.Tx) error {
		ok, err := tx.UpdateRebase(ctx, r, seen)
		if err != nil || !ok {
			return err
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "effort.rebase.abort", eff.Slug, nil, map[string]string{"rebase": r.ID.String(), "why": "automatic rebase conflicts"})
	}))
}

func ontoSHA(head store.ChangesetRow) string {
	if head.GitSHA != "" {
		return short(head.GitSHA)
	}
	return "vx " + head.Hash.Short()
}

func conflictPaths(cs []engine.Conflict) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return uniqueSorted(out)
}
