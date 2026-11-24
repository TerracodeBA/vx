package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
)

// RebaseState is the lifecycle state of an effort rebase.
type RebaseState string

const (
	RebaseRunning RebaseState = "running" // the server is replaying changes
	RebaseStopped RebaseState = "stopped" // a change conflicts and waits for a resolution
	RebaseDone    RebaseState = "done"    // the effort head moved to the rebased chain
	RebaseAborted RebaseState = "aborted" // dropped; the effort is unchanged
)

// Active reports whether the rebase is still in progress.
func (s RebaseState) Active() bool { return s == RebaseRunning || s == RebaseStopped }

// Replayed pairs an old-chain changeset with its rebased version.
type Replayed struct {
	From hash.Hash `json:"from"`
	To   hash.Hash `json:"to"`
}

// RebaseConflict is one path that stopped a rebase.
type RebaseConflict struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Generated bool   `json:"generated,omitempty"` // the file says "Code generated ... DO NOT EDIT."
}

// Rebase is one rebase of an effort onto a newer head of a branch.
type Rebase struct {
	ID         uuid.UUID
	RepoID     uuid.UUID
	EffortID   uuid.UUID
	State      RebaseState
	OntoBranch string
	Onto       hash.Hash
	OntoGitSHA string
	OldBase    hash.Hash
	OldHead    hash.Hash
	SeenHead   hash.Hash
	NewHead    hash.Hash
	Replayed   []Replayed
	Pending    []hash.Hash
	Conflicts  []RebaseConflict
	StartedBy  uuid.UUID
	UpdatedBy  uuid.UUID
	Version    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const rebaseCols = `id, repo_id, effort_id, state, onto_branch, onto, onto_git_sha, old_base, old_head, seen_head, new_head,
	replayed, pending, conflicts, started_by, updated_by, version, created_at, updated_at`

func scanRebase(row interface{ Scan(...any) error }) (Rebase, error) {
	var r Rebase
	var replayed, pending, conflicts []byte
	err := row.Scan(&r.ID, &r.RepoID, &r.EffortID, &r.State, &r.OntoBranch, &r.Onto, &r.OntoGitSHA, &r.OldBase, &r.OldHead,
		&r.SeenHead, &r.NewHead, &replayed, &pending, &conflicts, &r.StartedBy, &r.UpdatedBy, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(replayed, &r.Replayed); err != nil {
		return r, fmt.Errorf("rebase %s: replayed: %w", r.ID, err)
	}
	if err := json.Unmarshal(pending, &r.Pending); err != nil {
		return r, fmt.Errorf("rebase %s: pending: %w", r.ID, err)
	}
	if conflicts != nil {
		if err := json.Unmarshal(conflicts, &r.Conflicts); err != nil {
			return r, fmt.Errorf("rebase %s: conflicts: %w", r.ID, err)
		}
	}
	return r, nil
}

func rebaseJSON(r Rebase) (replayed, pending, conflicts []byte, err error) {
	if r.Replayed == nil {
		r.Replayed = []Replayed{}
	}
	if r.Pending == nil {
		r.Pending = []hash.Hash{}
	}
	if replayed, err = json.Marshal(r.Replayed); err != nil {
		return
	}
	if pending, err = json.Marshal(r.Pending); err != nil {
		return
	}
	if len(r.Conflicts) > 0 {
		conflicts, err = json.Marshal(r.Conflicts)
	}
	return
}

// StartRebase inserts a running rebase. It returns ErrExists when the
// effort already has a rebase running or stopped.
func (q Q) StartRebase(ctx context.Context, r Rebase) (Rebase, error) {
	replayed, pending, conflicts, err := rebaseJSON(r)
	if err != nil {
		return Rebase{}, err
	}
	out, err := scanRebase(q.db.QueryRow(ctx, `insert into effort_rebases
		(repo_id, effort_id, state, onto_branch, onto, onto_git_sha, old_base, old_head, seen_head, new_head,
		 replayed, pending, conflicts, started_by, updated_by)
		values ($1, $2, 'running', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
		returning `+rebaseCols,
		r.RepoID, r.EffortID, r.OntoBranch, r.Onto, r.OntoGitSHA, r.OldBase, r.OldHead, r.SeenHead, r.NewHead,
		replayed, pending, conflicts, r.StartedBy))
	if isUniqueViolation(err) {
		return Rebase{}, fmt.Errorf("a rebase of this effort is already in progress: %w", ErrExists)
	}
	return out, err
}

// UpdateRebase saves a rebase's progress with a compare-and-swap on its
// version, and reports false when someone else changed it first (another
// continue, or an abort).
func (q Q) UpdateRebase(ctx context.Context, r Rebase, seenVersion int64) (bool, error) {
	replayed, pending, conflicts, err := rebaseJSON(r)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `update effort_rebases set state = $1, seen_head = $2, new_head = $3, replayed = $4,
		pending = $5, conflicts = $6, updated_by = $7, version = version + 1, updated_at = now()
		where id = $8 and version = $9 and state in ('running', 'stopped')`,
		r.State, r.SeenHead, r.NewHead, replayed, pending, conflicts, r.UpdatedBy, r.ID, seenVersion)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ActiveRebase returns the effort's running or stopped rebase.
func (q Q) ActiveRebase(ctx context.Context, effortID uuid.UUID) (Rebase, error) {
	r, err := scanRebase(q.db.QueryRow(ctx, `select `+rebaseCols+` from effort_rebases
		where effort_id = $1 and state in ('running', 'stopped')`, effortID))
	return r, notFound(err, "rebase in progress")
}

// LatestRebase returns the effort's newest rebase in any state.
func (q Q) LatestRebase(ctx context.Context, effortID uuid.UUID) (Rebase, error) {
	r, err := scanRebase(q.db.QueryRow(ctx, `select `+rebaseCols+` from effort_rebases
		where effort_id = $1 order by created_at desc, id limit 1`, effortID))
	return r, notFound(err, "rebase")
}

// FinishedRebaseHeads returns, newest first, the old-chain heads of the
// effort's finished rebases: what open workspaces may still be based on.
func (q Q) FinishedRebaseHeads(ctx context.Context, effortID uuid.UUID) ([]hash.Hash, error) {
	rows, err := q.db.Query(ctx, `select seen_head from effort_rebases where effort_id = $1 and state = 'done'
		order by created_at desc`, effortID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hash.Hash
	for rows.Next() {
		var h hash.Hash
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// AdoptChangesets marks changesets as part of an effort; a rebase stores
// its replayed changes without an effort until it finishes.
func (q Q) AdoptChangesets(ctx context.Context, repoID, effortID uuid.UUID, hs []hash.Hash) error {
	if len(hs) == 0 {
		return nil
	}
	raw := make([][]byte, 0, len(hs))
	for _, h := range hs {
		raw = append(raw, h.Bytes())
	}
	_, err := q.db.Exec(ctx, `update changesets set effort_id = $2 where repo_id = $1 and hash = any($3)`, repoID, effortID, raw)
	return err
}
