package service

// Landing policies (#6): vx.toml [[policy]] says how changes to some paths
// land. fast lands at once (what vx always did); gated lands only when
// every required check ran by vx and passed in this land; human is gated
// and then waits for a person: the land is stored as a pending review and
// the effort head does not move until someone approves it.
//
// Who approves: a person (never an agent) with write on the repository
// (on a GitHub-backed repository: write on GitHub), who is in every
// approvers list of the matching human policies when they set one, and who
// is not the author of the change. A person may approve the changes of
// their own agents: the policy asks for a person to look, and agents act
// for their person. A change a person landed by hand needs another person.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// NoteApproval is a system note about a review under a human landing
// policy: waiting, approved, rejected, or conflicting on approval. Notes to
// the author are addressed to them (Note.To).
const NoteApproval NoteKind = "approval"

// MaxReviewReason is the longest rejection reason.
const MaxReviewReason = 1000

// Review is a land waiting for a person, with who it is by and, for the
// caller, whether they may decide it.
type Review struct {
	store.Review
	RepoName    string
	EffortSlug  string
	Author      store.Principal
	AuthorOwner *store.Principal // the person an agent author acts for
	DecidedBy   *store.Principal
	// Files are the changes of the snapshot against its base (GetReview
	// only).
	Files []store.FileChange
	// CanDecide says whether the caller may approve or reject it; when not,
	// CannotDecide says why.
	CanDecide    bool
	CannotDecide string
}

// WaitingText is what the author's vx land prints:
//
//	Waiting for a person to review: internal/store/migrations/021_x.sql (policy in vx.toml)
func (r Review) WaitingText() string {
	return "Waiting for a person to review: " + strings.Join(r.Paths, ", ") + " (policy in " + rules.File + ")"
}

// landPolicy decides the landing policy of a land: the strictest matching
// [[policy]] of the vx.toml at the effort head and at the snapshot, for the
// files the snapshot changes against its base, so a change can never weaken
// its own policy. For gated and human it refuses the land unless every
// required check ran by vx and passed.
func (s *Service) landPolicy(ctx context.Context, v WorkspaceView, base store.ChangesetRow, in LandInput) (rules.Decision, error) {
	changes, err := object.DiffTrees(ctx, s.Blobs, base.Tree, in.Tree)
	if err != nil {
		return rules.Decision{}, err
	}
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	var ds []rules.Decision
	trees := []hash.Hash{in.Tree}
	if head, err := s.Store.GetChangeset(ctx, v.Repo.ID, v.Effort.Head); err == nil {
		trees = append(trees, head.Tree)
	} else {
		return rules.Decision{}, classify(err)
	}
	for _, t := range trees {
		r, err := rules.At(ctx, s.Blobs, t)
		if err != nil {
			continue // a vx.toml that does not parse has no policies; the land warns about it
		}
		ds = append(ds, r.Decide(paths))
	}
	d := rules.Combine(ds...)
	if d.Mode == rules.ModeFast {
		return d, nil
	}
	var passed, failed []string
	if in.Evidence != nil {
		for _, c := range in.Evidence.Checks {
			if !c.RanByVX {
				continue
			}
			if c.Result == "pass" {
				passed = append(passed, c.Name)
			} else {
				failed = append(failed, c.Name)
			}
		}
	}
	missing := d.Missing(passed, failed)
	if len(missing) == 0 {
		return d, nil
	}
	return d, &Error{Code: CodeFailedPrecondition, Msg: policyRefusal(d, missing)}
}

// policyRefusal says why a gated or human land was refused and how to land:
//
//	not landed: internal/api/x.go is under a gated landing policy ([[policy]] paths = ["internal/api/**"] (vx.toml line 8)).
//	It needs every required check run by vx (vx land --run) and passing in this land:
//	  go vet ./...   (not run by vx)
//	  go test ./...  (failed)
//	Land with:
//	  vx land --run 'go vet ./...' --run 'go test ./...'
func policyRefusal(d rules.Decision, missing []rules.MissingCheck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "not landed: %s %s under a %s landing policy (%s).\n", joinAnd(d.Paths), isAre(len(d.Paths)), d.Mode, strings.Join(d.Policies, "; "))
	if d.Mode == rules.ModeHuman {
		b.WriteString("Before a person reviews it, every required check must run by vx (vx land --run) and pass in this land:\n")
	} else {
		b.WriteString("It needs every required check run by vx (vx land --run) and passing in this land:\n")
	}
	width := 0
	for _, m := range missing {
		width = max(width, len(m.Command))
	}
	for _, m := range missing {
		why := "not run by vx"
		if m.Failed {
			why = "failed"
		}
		fmt.Fprintf(&b, "  %-*s  (%s)\n", width, m.Command, why)
	}
	b.WriteString("Land with:\n  " + d.LandCommand())
	return b.String()
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// submitReview stores a land under a human policy as the workspace's
// pending review, after checking that it merges onto the current effort
// head: conflicts come back like a land's, so the author resolves them
// before a person looks. The effort head does not move.
func (s *Service) submitReview(ctx context.Context, actor Actor, v WorkspaceView, base store.ChangesetRow, in LandInput, intent string, d rules.Decision) (LandResult, error) {
	eff, err := s.Store.EffortByID(ctx, v.Effort.ID)
	if err != nil {
		return LandResult{}, classify(err)
	}
	if eff.State != store.EffortOpen {
		return LandResult{}, errf(CodeFailedPrecondition, "effort %s is %s", eff.Slug, eff.State)
	}
	head, err := s.Store.GetChangeset(ctx, v.Repo.ID, eff.Head)
	if err != nil {
		return LandResult{}, err
	}
	res := LandResult{EffortHead: eff.Head}
	if in.RequireUpToDate && eff.Head != base.Hash {
		res.EffortMoved = true
		return res, nil
	}
	merged := in.Tree
	if eff.Head != base.Hash {
		mr, err := engine.MergeTrees(ctx, s.repoWrites(v.Repo.ID), base.Tree, in.Tree, head.Tree, merge.Labels{
			Base: "base " + base.Hash.Short(), Ours: "workspace", Theirs: "effort " + eff.Head.Short(),
		}, s.mergeOptions(ctx, in.Tree, head.Tree)...)
		if err != nil {
			return LandResult{}, err
		}
		if !mr.Clean() {
			res.Conflicts = mr.Conflicts
			return res, nil
		}
		merged = mr.Tree
	}
	if merged == head.Tree {
		return LandResult{}, &Error{Code: CodeFailedPrecondition, Msg: "nothing to land: the effort already has these changes", Err: ErrNothingToLand}
	}
	vx, err := s.systemPrincipal(ctx)
	if err != nil {
		return LandResult{}, err
	}
	var rv store.Review
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		ws, err := tx.GetWorkspaceForUpdate(ctx, v.Workspace.ID)
		if err != nil {
			return err
		}
		if ws.State != store.WorkspaceOpen {
			return errf(CodeFailedPrecondition, "workspace is already %s", ws.State)
		}
		_, prevErr := tx.PendingReviewOf(ctx, ws.ID)
		first := errors.Is(prevErr, store.ErrNotFound)
		if prevErr != nil && !first {
			return prevErr
		}
		rv, err = tx.SubmitReview(ctx, store.Review{
			RepoID: v.Repo.ID, EffortID: eff.ID, WorkspaceID: ws.ID, AuthorID: actor.ID, ChangeID: ws.ChangeID,
			Intent: intent, Base: base.Hash, Tree: in.Tree, Evidence: in.Evidence, DependsOn: in.DependsOn,
			Model: in.Model, Session: in.Session, Paths: d.Paths, Policies: d.Policies, Approvers: d.Approvers,
		})
		if err != nil {
			return err
		}
		if first {
			body := fmt.Sprintf("Waiting for a person to review: %s by %s (%q) changes %s, under a human landing policy in %s. Approve or reject it in the web UI (Reviews) or with vx review approve %s / vx review reject %s --reason TEXT.",
				ws.ChangeID, actor.Ref(), reviewTitle(intent), joinAnd(d.Paths), rules.File, shortID(rv.ID), shortID(rv.ID))
			if _, err := tx.InsertNote(ctx, store.Note{EffortID: eff.ID, Kind: string(NoteApproval), Targets: noteTargets(d.Paths),
				AuthorID: vx.ID, Body: clip(body, MaxNoteBody), State: NoteOpen, CreatedAt: s.Clock(), System: true}); err != nil {
				return err
			}
		}
		return tx.LogOp(ctx, &v.Repo.ID, &actor.ID, "review.submit", rv.ID.String(), nil,
			map[string]any{"effort": eff.Slug, "workspace": ws.ID.String(), "paths": d.Paths, "tree": in.Tree.String()})
	})
	if err != nil {
		return LandResult{}, classify(err)
	}
	view, err := s.reviewView(ctx, actor, rv, v.Repo.Name, eff.Slug)
	if err != nil {
		return LandResult{}, err
	}
	res.Review = &view
	return res, nil
}

// noteTargets are a note's targets for paths: at most MaxNoteTargets.
func noteTargets(paths []string) []string {
	if len(paths) > MaxNoteTargets {
		return append([]string(nil), paths[:MaxNoteTargets]...)
	}
	return append([]string(nil), paths...)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// reviewView fills in a review's people and, for actor, whether they may
// decide it.
func (s *Service) reviewView(ctx context.Context, actor Actor, r store.Review, repoName, effortSlug string) (Review, error) {
	out := Review{Review: r, RepoName: repoName, EffortSlug: effortSlug}
	author, err := s.Store.PrincipalByID(ctx, r.AuthorID)
	if err != nil {
		return out, err
	}
	out.Author = author
	if author.ParentID != nil {
		if p, err := s.Store.PrincipalByID(ctx, *author.ParentID); err == nil {
			out.AuthorOwner = &p
		}
	}
	if r.DecidedBy != nil {
		if p, err := s.Store.PrincipalByID(ctx, *r.DecidedBy); err == nil {
			out.DecidedBy = &p
		}
	}
	if r.State == store.ReviewPending {
		if why := s.cannotDecide(ctx, actor, r); why != "" {
			out.CannotDecide = why
		} else {
			out.CanDecide = true
		}
	}
	return out, nil
}

// cannotDecide says why actor may not approve or reject r, or "" when they
// may. The caller has already checked that actor can read the repository.
func (s *Service) cannotDecide(ctx context.Context, actor Actor, r store.Review) string {
	if actor.Kind != store.KindHuman {
		return "only people approve or reject reviews; an agent cannot, even for its own person"
	}
	if actor.ID == r.AuthorID {
		return "you wrote this change yourself; another person has to review it"
	}
	if _, err := s.AuthorizeID(ctx, actor, r.RepoID, RoleWrite); err != nil {
		return "approving needs write on the repository: " + errorText(err)
	}
	for _, list := range r.Approvers {
		if !s.inApprovers(ctx, actor, list) {
			return "the landing policy names its approvers and you are not one of them: " + strings.Join(list, ", ")
		}
	}
	return ""
}

func errorText(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return err.Error()
}

// inApprovers reports whether actor is in a policy's approvers list:
// user:HANDLE is the person with that handle, team:ORG/TEAM a member of
// that vx team.
func (s *Service) inApprovers(ctx context.Context, actor Actor, list []string) bool {
	for _, entry := range list {
		a, err := rules.ParseApprover(entry)
		if err != nil {
			continue
		}
		switch a.Kind {
		case "user":
			if a.Name == actor.Handle {
				return true
			}
		case "team":
			orgName, teamName, _ := strings.Cut(a.Name, "/")
			org, err := s.Store.OrgByHandle(ctx, orgName)
			if err != nil {
				continue
			}
			team, err := s.Store.TeamByName(ctx, org, teamName)
			if err != nil {
				continue
			}
			for _, m := range team.Members {
				if m.ID == actor.ID {
					return true
				}
			}
		}
	}
	return false
}

// ReviewQuery filters ListReviews.
type ReviewQuery struct {
	Repo   string // empty: every repository the caller can read
	Effort string // needs Repo
	// States: empty means pending only.
	States []store.ReviewState
	Limit  int
}

// ListReviews returns reviews in the repositories the caller can read
// (read), newest first: pending ones unless States says otherwise.
func (s *Service) ListReviews(ctx context.Context, actor Actor, q ReviewQuery) ([]Review, error) {
	f := store.ReviewFilter{States: q.States, Limit: q.Limit}
	if len(f.States) == 0 {
		f.States = []store.ReviewState{store.ReviewPending}
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 100
	}
	names := map[uuid.UUID]string{}
	switch {
	case q.Effort != "":
		v, err := s.effortFor(ctx, actor, q.Repo, q.Effort, RoleRead)
		if err != nil {
			return nil, err
		}
		f.Repos, f.EffortID = []uuid.UUID{v.Repo.ID}, v.Effort.ID
		names[v.Repo.ID] = v.Repo.Name
	case q.Repo != "":
		repo, _, err := s.Authorize(ctx, actor, q.Repo, RoleRead)
		if err != nil {
			return nil, err
		}
		f.Repos = []uuid.UUID{repo.ID}
		names[repo.ID] = repo.Name
	default:
		ids, all, err := s.ReadableRepoIDs(ctx, actor)
		if err != nil {
			return nil, err
		}
		if !all {
			if len(ids) == 0 {
				return nil, nil
			}
			f.Repos = ids
		}
	}
	rows, err := s.Store.ListReviews(ctx, f)
	if err != nil {
		return nil, err
	}
	slugs := map[uuid.UUID]string{}
	out := make([]Review, 0, len(rows))
	for _, r := range rows {
		repoName, ok := names[r.RepoID]
		if !ok {
			repo, err := s.Store.RepoByID(ctx, r.RepoID)
			if err != nil {
				return nil, err
			}
			repoName, names[r.RepoID] = repo.Name, repo.Name
		}
		slug, ok := slugs[r.EffortID]
		if !ok {
			eff, err := s.Store.EffortByID(ctx, r.EffortID)
			if err != nil {
				return nil, err
			}
			slug, slugs[r.EffortID] = eff.Slug, eff.Slug
		}
		v, err := s.reviewView(ctx, actor, r, repoName, slug)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// review resolves a review id (or an 8+ character prefix) the caller can
// read with need on its repository. One the caller cannot read is not
// found.
func (s *Service) review(ctx context.Context, actor Actor, id string, need RepoRole) (store.Review, store.Repo, store.Effort, error) {
	id = strings.TrimSpace(id)
	if len(id) < 8 {
		return store.Review{}, store.Repo{}, store.Effort{}, errf(CodeInvalid, "review id %q is too short: give at least 8 characters (vx review list shows them)", id)
	}
	ids, all, err := s.ReadableRepoIDs(ctx, actor)
	if err != nil {
		return store.Review{}, store.Repo{}, store.Effort{}, err
	}
	var repos []uuid.UUID
	if !all {
		repos = append([]uuid.UUID{}, ids...)
	}
	notFound := errf(CodeNotFound, "no review %s; vx review list shows the pending ones", id)
	r, err := s.Store.ReviewByPrefix(ctx, id, repos)
	if errors.Is(err, store.ErrNotFound) {
		return r, store.Repo{}, store.Effort{}, notFound
	}
	if err != nil {
		return r, store.Repo{}, store.Effort{}, classify(err)
	}
	if _, err := s.AuthorizeID(ctx, actor, r.RepoID, need); err != nil {
		if isHidden(err) {
			return r, store.Repo{}, store.Effort{}, notFound
		}
		return r, store.Repo{}, store.Effort{}, err
	}
	repo, err := s.Store.RepoByID(ctx, r.RepoID)
	if err != nil {
		return r, repo, store.Effort{}, err
	}
	eff, err := s.Store.EffortByID(ctx, r.EffortID)
	return r, repo, eff, classify(err)
}

// GetReview returns one review with the files its snapshot changes (read).
func (s *Service) GetReview(ctx context.Context, actor Actor, id string) (Review, error) {
	r, repo, eff, err := s.review(ctx, actor, id, RoleRead)
	if err != nil {
		return Review{}, err
	}
	v, err := s.reviewView(ctx, actor, r, repo.Name, eff.Slug)
	if err != nil {
		return Review{}, err
	}
	base, err := s.Store.GetChangeset(ctx, r.RepoID, r.Base)
	if err != nil {
		return Review{}, classify(err)
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, base.Tree, r.Tree)
	if err != nil {
		return Review{}, err
	}
	if v.Files, err = s.lineStats(ctx, changes); err != nil {
		return Review{}, err
	}
	return v, nil
}

// ReviewDecision is the outcome of ApproveReview or RejectReview.
type ReviewDecision struct {
	Review Review
	// Landed is set when an approval landed the change; Conflicts when it
	// could not because the change now conflicts with the effort (the
	// review stays pending and goes back to the author).
	Landed    *LandResult
	Conflicts []engine.Conflict
}

// ApproveReview approves a pending review and lands it onto the current
// effort head with the normal merge (write; a person, see cannotDecide).
// The approval is recorded in the change's evidence. When the merge
// conflicts the review stays pending and a note asks the author to
// resolve and land again.
func (s *Service) ApproveReview(ctx context.Context, actor Actor, id string) (ReviewDecision, error) {
	r, repo, eff, err := s.review(ctx, actor, id, RoleWrite)
	if err != nil {
		return ReviewDecision{}, err
	}
	if err := s.checkDecide(ctx, actor, r); err != nil {
		return ReviewDecision{}, err
	}
	if err := s.requireAgreement(ctx, repo, "approving reviews, which lands them"); err != nil {
		return ReviewDecision{}, err
	}
	author, err := s.Store.PrincipalByID(ctx, r.AuthorID)
	if err != nil {
		return ReviewDecision{}, err
	}
	authorActor, err := s.ResolveActor(ctx, author)
	if err != nil {
		return ReviewDecision{}, err
	}
	v, err := s.loadWorkspace(ctx, r.WorkspaceID)
	if err != nil {
		return ReviewDecision{}, err
	}
	base, err := s.Store.GetChangeset(ctx, repo.ID, r.Base)
	if err != nil {
		return ReviewDecision{}, classify(err)
	}
	now := s.Clock().UTC().Truncate(time.Microsecond)
	ev := &object.Evidence{}
	if r.Evidence != nil {
		*ev = *r.Evidence
	}
	ev.Approvals = append(append([]object.Approval(nil), ev.Approvals...), object.Approval{By: actor.Ref(), At: now, Review: r.ID.String(), Paths: r.Paths})
	in := LandInput{WorkspaceID: r.WorkspaceID, Base: r.Base, Tree: r.Tree, Evidence: ev, DependsOn: r.DependsOn, Model: r.Model, Session: r.Session}
	extra := &landExtra{inTx: func(tx *store.Tx, landed hash.Hash) error {
		ok, err := tx.DecideReview(ctx, r.ID, store.ReviewApproved, actor.ID, "", landed, now)
		if err != nil {
			return err
		}
		if !ok {
			return errf(CodeFailedPrecondition, "review %s was decided meanwhile", shortID(r.ID))
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "review.approve", r.ID.String(), nil, map[string]any{"landed": landed.String()})
	}}
	attempts := s.MaxLandAttempts
	if attempts <= 0 {
		attempts = 25
	}
	var res LandResult
	for attempt := 0; ; attempt++ {
		if attempt == attempts {
			return ReviewDecision{}, errf(CodeUnavailable, "effort %s is too busy: gave up after %d attempts, try again", eff.Slug, attempts)
		}
		res, err = s.landOnce(ctx, authorActor, v, base, in, r.Intent, extra)
		if errors.Is(err, errLostRace) {
			continue
		}
		if err != nil {
			return ReviewDecision{}, err
		}
		break
	}
	if !res.Landed {
		var paths []string
		for _, c := range res.Conflicts {
			paths = append(paths, c.Path)
		}
		sort.Strings(paths)
		if err := s.Store.SetReviewConflicts(ctx, r.ID, paths, actor.ID, now); err != nil {
			return ReviewDecision{}, err
		}
		s.noteAuthor(ctx, repo, eff, r, fmt.Sprintf("%s approved your change %s (%q), but it no longer merges with the effort: conflicts in %s. Run vx sync, resolve them, then vx land again; the review stays pending and a person approves the new snapshot.",
			actor.Ref(), r.ChangeID, reviewTitle(r.Intent), joinAnd(paths)), paths)
		r.Conflicts, r.DecidedBy, r.DecidedAt = paths, &actor.ID, &now
		view, err := s.reviewView(ctx, actor, r, repo.Name, eff.Slug)
		return ReviewDecision{Review: view, Conflicts: res.Conflicts}, err
	}
	if res.Overlaps, err = s.storedOverlaps(ctx, repo, eff.ID); err != nil {
		slog.Warn("overlaps after approval", "repo", repo.Name, "effort", eff.Slug, "err", err)
		res.Overlaps = nil
	}
	s.scheduleForecast(repo, eff.ID)
	s.noteAuthor(ctx, repo, eff, r, fmt.Sprintf("%s approved your change %s (%q); it landed as %s. Run vx sync to bring it in.",
		actor.Ref(), r.ChangeID, reviewTitle(r.Intent), res.Changeset.Hash.Short()), r.Paths)
	r.State, r.DecidedBy, r.DecidedAt, r.Landed = store.ReviewApproved, &actor.ID, &now, res.Changeset.Hash
	view, err := s.reviewView(ctx, actor, r, repo.Name, eff.Slug)
	return ReviewDecision{Review: view, Landed: &res}, err
}

// RejectReview rejects a pending review with a reason (write; a person,
// see cannotDecide). The author's workspace stays open: they change the
// files and land again, which opens a new review, or abandon it.
func (s *Service) RejectReview(ctx context.Context, actor Actor, id, reason string) (ReviewDecision, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ReviewDecision{}, errf(CodeInvalid, "say why you reject it: the author reads the reason (vx review reject ID --reason TEXT)")
	}
	if len(reason) > MaxReviewReason {
		return ReviewDecision{}, errf(CodeInvalid, "the reason is %d characters; keep it under %d", len(reason), MaxReviewReason)
	}
	r, repo, eff, err := s.review(ctx, actor, id, RoleWrite)
	if err != nil {
		return ReviewDecision{}, err
	}
	if err := s.checkDecide(ctx, actor, r); err != nil {
		return ReviewDecision{}, err
	}
	now := s.Clock().UTC().Truncate(time.Microsecond)
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		ok, err := tx.DecideReview(ctx, r.ID, store.ReviewRejected, actor.ID, reason, hash.Zero, now)
		if err != nil {
			return err
		}
		if !ok {
			return errf(CodeFailedPrecondition, "review %s was decided meanwhile", shortID(r.ID))
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "review.reject", r.ID.String(), nil, map[string]any{"reason": reason})
	})
	if err != nil {
		return ReviewDecision{}, classify(err)
	}
	s.noteAuthor(ctx, repo, eff, r, fmt.Sprintf("%s rejected your change %s (%q): %s. Your workspace stays open: change the files and vx land again for a new review, or vx abandon.",
		actor.Ref(), r.ChangeID, reviewTitle(r.Intent), reason), r.Paths)
	r.State, r.DecidedBy, r.DecidedAt, r.Reason = store.ReviewRejected, &actor.ID, &now, reason
	view, err := s.reviewView(ctx, actor, r, repo.Name, eff.Slug)
	return ReviewDecision{Review: view}, err
}

// checkDecide refuses a decision on a review that is not pending or that
// actor may not decide.
func (s *Service) checkDecide(ctx context.Context, actor Actor, r store.Review) error {
	if r.State != store.ReviewPending {
		return errf(CodeFailedPrecondition, "review %s is already %s", shortID(r.ID), r.State)
	}
	if why := s.cannotDecide(ctx, actor, r); why != "" {
		return errf(CodePermission, "%s", why)
	}
	return nil
}

// noteAuthor posts a system note to the author of a review. It never
// fails the decision: a note that cannot be posted is logged.
func (s *Service) noteAuthor(ctx context.Context, repo store.Repo, eff store.Effort, r store.Review, body string, paths []string) {
	vx, err := s.systemPrincipal(ctx)
	if err == nil {
		to := r.AuthorID
		_, err = s.Store.InsertNote(ctx, store.Note{EffortID: eff.ID, Kind: string(NoteApproval), Targets: noteTargets(paths),
			AuthorID: vx.ID, ToID: &to, Body: clip(body, MaxNoteBody), State: NoteOpen, CreatedAt: s.Clock(), System: true})
	}
	if err != nil {
		slog.Warn("review note", "repo", repo.Name, "effort", eff.Slug, "review", r.ID, "err", err)
	}
}

// reviewTitle is the first line of an intent.
func reviewTitle(intent string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(intent), "\n")
	return line
}
