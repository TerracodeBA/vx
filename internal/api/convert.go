package api

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func tsp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}

func tokenPB(t store.Token, owner string, current uuid.UUID) *vxv1.Token {
	return &vxv1.Token{
		Id: t.ID.String(), Principal: owner, Name: t.Name, CreatedAt: ts(t.CreatedAt),
		LastUsedAt: tsp(t.LastUsedAt), RevokedAt: tsp(t.RevokedAt), Current: current != uuid.Nil && t.ID == current,
	}
}

func principalPB(a service.Actor) *vxv1.Principal {
	return &vxv1.Principal{
		Ref: a.Ref(), Handle: a.Handle, Kind: string(a.Kind), DisplayName: a.DisplayName,
		Email: a.Email, OnBehalfOf: a.OnBehalfOf(),
	}
}

func repoPB(r store.Repo, branches []store.Branch) *vxv1.Repo {
	out := &vxv1.Repo{Name: r.Name, Provider: string(r.Provider), Remote: r.Remote, DefaultBranch: r.DefaultBranch}
	for _, b := range branches {
		out.Branches = append(out.Branches, branchPB(b))
	}
	return out
}

func branchPB(b store.Branch) *vxv1.Branch {
	return &vxv1.Branch{Name: b.Name, Head: b.Head.String(), GitSha: b.GitSHA, SyncedAt: ts(b.SyncedAt)}
}

func effortPB(repo string, e store.Effort, owner string, open int) *vxv1.Effort {
	return &vxv1.Effort{
		Repo: repo, Slug: e.Slug, Name: e.Name, Goal: e.Goal, BaseBranch: e.BaseBranch,
		Base: e.Base.String(), Head: e.Head.String(), Version: e.Version, State: string(e.State),
		Owner: owner, OpenWorkspaces: int32(open), CreatedAt: ts(e.CreatedAt), UpdatedAt: ts(e.UpdatedAt),
	}
}

func workspacePB(repo, effort string, w store.Workspace, owner string) *vxv1.Workspace {
	return &vxv1.Workspace{
		Id: w.ID.String(), Repo: repo, Effort: effort, Intent: w.Intent, ChangeId: w.ChangeID,
		Base: w.Base.String(), State: string(w.State), Owner: owner, LandedAs: w.LandedAs.String(),
		CreatedAt: ts(w.CreatedAt),
	}
}

func evidencePB(e *object.Evidence) *vxv1.Evidence {
	if e == nil || e.IsEmpty() {
		return nil
	}
	out := &vxv1.Evidence{NewTests: e.NewTests}
	for _, c := range e.Checks {
		out.Checks = append(out.Checks, &vxv1.Check{Name: c.Name, Result: c.Result, Passed: int32(c.Passed), Failed: int32(c.Failed), RanByVx: c.RanByVX, DurationMs: c.DurationMs, Notes: c.Notes})
	}
	return out
}

func evidenceFromPB(e *vxv1.Evidence) (*object.Evidence, error) {
	if e == nil {
		return nil, nil
	}
	out := &object.Evidence{NewTests: e.NewTests}
	for _, c := range e.Checks {
		if c.Result != "pass" && c.Result != "fail" {
			return nil, fmt.Errorf("check %q: result must be pass or fail", c.Name)
		}
		out.Checks = append(out.Checks, object.Check{Name: c.Name, Result: c.Result, Passed: int(c.Passed), Failed: int(c.Failed), RanByVX: c.RanByVx, DurationMs: c.DurationMs, Notes: c.Notes})
	}
	if out.IsEmpty() {
		return nil, nil
	}
	return out, nil
}

func fileChangesPB(files []store.FileChange) []*vxv1.FileChange {
	out := make([]*vxv1.FileChange, 0, len(files))
	for _, f := range files {
		mode := f.New.Mode
		if f.Op == object.OpDelete {
			mode = f.Old.Mode
		}
		out = append(out, &vxv1.FileChange{
			Path: f.Path, Op: string(f.Op), OldHash: f.Old.Hash.String(), NewHash: f.New.Hash.String(),
			Mode: mode.String(), Added: int32(f.Added), Deleted: int32(f.Deleted),
		})
	}
	return out
}

func changesetPB(h hash.Hash, cs *object.Changeset, effort string, files []store.FileChange) *vxv1.Changeset {
	out := &vxv1.Changeset{
		Hash: h.String(), ChangeId: cs.ChangeID, Tree: cs.Tree.String(), Origin: string(cs.Origin), GitSha: cs.GitSHA,
		Author: &vxv1.Author{
			Principal: cs.Author.Principal, Name: cs.Author.Name, Email: cs.Author.Email,
			OnBehalfOf: cs.Author.OnBehalfOf, Model: cs.Author.Model, Session: cs.Author.Session,
		},
		Intent: cs.Intent, DependsOn: cs.DependsOn, Evidence: evidencePB(cs.Evidence),
		CreatedAt: ts(cs.CreatedAt), Effort: effort,
		SnapshotTree: cs.SnapshotTree.String(), Merged: cs.Merged(),
	}
	for _, p := range cs.Parents {
		out.Parents = append(out.Parents, p.String())
	}
	if files != nil {
		out.Files = fileChangesPB(files)
	}
	return out
}

func conflictsPB(cs []engine.Conflict) []*vxv1.Conflict {
	out := make([]*vxv1.Conflict, 0, len(cs))
	for _, c := range cs {
		out = append(out, &vxv1.Conflict{
			Path: c.Path, Kind: string(c.Kind), Detail: c.Detail,
			Base: c.Base.Hash.String(), Ours: c.Ours.Hash.String(), Theirs: c.Theirs.Hash.String(),
		})
	}
	return out
}

func warningsPB(ws []engine.Warning) []*vxv1.Warning {
	out := make([]*vxv1.Warning, 0, len(ws))
	for _, w := range ws {
		out = append(out, &vxv1.Warning{Path: w.Path, Kind: string(w.Kind), Detail: w.Detail})
	}
	return out
}

func parseHash(field, s string) (hash.Hash, error) {
	h, err := hash.Parse(s)
	if err != nil {
		return hash.Zero, fmt.Errorf("%s: %w", field, err)
	}
	return h, nil
}
