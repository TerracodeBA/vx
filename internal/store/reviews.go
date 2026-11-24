package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// ReviewState is the lifecycle state of a review.
type ReviewState string

const (
	ReviewPending   ReviewState = "pending"   // waits for a person
	ReviewApproved  ReviewState = "approved"  // a person approved it and it landed
	ReviewRejected  ReviewState = "rejected"  // a person rejected it; the workspace stays open
	ReviewWithdrawn ReviewState = "withdrawn" // the workspace was abandoned
)

// Review is a land under a "human" landing policy that waits for a person:
// the author's snapshot, kept until someone approves or rejects it.
type Review struct {
	ID          uuid.UUID
	RepoID      uuid.UUID
	EffortID    uuid.UUID
	WorkspaceID uuid.UUID
	AuthorID    uuid.UUID
	ChangeID    string
	Intent      string
	Base        hash.Hash // changeset the snapshot was made on
	Tree        hash.Hash // the snapshot
	Evidence    *object.Evidence
	DependsOn   []string
	Model       string
	Session     string
	Paths       []string   // changed files under the human policy
	Policies    []string   // the matching [[policy]] tables, for people
	Approvers   [][]string // a person must be in every list; empty: anyone with write
	State       ReviewState
	Conflicts   []string // paths that conflicted when it was last approved
	DecidedBy   *uuid.UUID
	DecidedAt   *time.Time
	Reason      string
	Landed      hash.Hash
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const reviewCols = `id, repo_id, effort_id, workspace_id, author_id, change_id, intent, base, tree, evidence, depends_on,
	model, session, paths, policies, approvers, state, conflicts, decided_by, decided_at, reason, landed, created_at, updated_at`

func scanReview(row interface{ Scan(...any) error }) (Review, error) {
	var r Review
	var ev, approvers []byte
	err := row.Scan(&r.ID, &r.RepoID, &r.EffortID, &r.WorkspaceID, &r.AuthorID, &r.ChangeID, &r.Intent, &r.Base, &r.Tree,
		&ev, &r.DependsOn, &r.Model, &r.Session, &r.Paths, &r.Policies, &approvers, &r.State, &r.Conflicts,
		&r.DecidedBy, &r.DecidedAt, &r.Reason, &r.Landed, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	if len(ev) > 0 && string(ev) != "null" {
		r.Evidence = &object.Evidence{}
		if err := json.Unmarshal(ev, r.Evidence); err != nil {
			return r, fmt.Errorf("review %s: evidence: %w", r.ID, err)
		}
	}
	if err := json.Unmarshal(approvers, &r.Approvers); err != nil {
		return r, fmt.Errorf("review %s: approvers: %w", r.ID, err)
	}
	return r, nil
}

func reviewJSON(r Review) (ev, approvers []byte, err error) {
	if r.Evidence != nil && !r.Evidence.IsEmpty() {
		if ev, err = json.Marshal(r.Evidence); err != nil {
			return nil, nil, err
		}
	}
	if r.Approvers == nil {
		r.Approvers = [][]string{}
	}
	approvers, err = json.Marshal(r.Approvers)
	return ev, approvers, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// SubmitReview stores r as the workspace's pending review: a new one, or
// in place of the workspace's pending review (a later land from the same
// workspace replaces the snapshot and clears the conflicts). It returns
// the stored review.
func (q Q) SubmitReview(ctx context.Context, r Review) (Review, error) {
	ev, approvers, err := reviewJSON(r)
	if err != nil {
		return Review{}, err
	}
	out, err := scanReview(q.db.QueryRow(ctx, `update reviews set intent = $2, base = $3, tree = $4, evidence = $5,
		depends_on = $6, model = $7, session = $8, paths = $9, policies = $10, approvers = $11, conflicts = '{}',
		decided_by = null, decided_at = null, change_id = $12, updated_at = now()
		where workspace_id = $1 and state = 'pending' returning `+reviewCols,
		r.WorkspaceID, r.Intent, r.Base, r.Tree, ev, nonNil(r.DependsOn), r.Model, r.Session, nonNil(r.Paths),
		nonNil(r.Policies), approvers, r.ChangeID))
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Review{}, err
	}
	return scanReview(q.db.QueryRow(ctx, `insert into reviews (repo_id, effort_id, workspace_id, author_id, change_id, intent,
		base, tree, evidence, depends_on, model, session, paths, policies, approvers, state)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'pending') returning `+reviewCols,
		r.RepoID, r.EffortID, r.WorkspaceID, r.AuthorID, r.ChangeID, r.Intent, r.Base, r.Tree, ev, nonNil(r.DependsOn),
		r.Model, r.Session, nonNil(r.Paths), nonNil(r.Policies), approvers))
}

// GetReview loads a review by id.
func (q Q) GetReview(ctx context.Context, id uuid.UUID) (Review, error) {
	r, err := scanReview(q.db.QueryRow(ctx, `select `+reviewCols+` from reviews where id = $1`, id))
	return r, notFound(err, "review")
}

// GetReviewForUpdate loads and locks a review (in a transaction).
func (q Q) GetReviewForUpdate(ctx context.Context, id uuid.UUID) (Review, error) {
	r, err := scanReview(q.db.QueryRow(ctx, `select `+reviewCols+` from reviews where id = $1 for update`, id))
	return r, notFound(err, "review")
}

// ReviewByPrefix resolves a review id or a prefix of at least 8 hex
// characters among reviews of the given repositories (nil: any).
func (q Q) ReviewByPrefix(ctx context.Context, prefix string, repos []uuid.UUID) (Review, error) {
	if id, err := uuid.Parse(prefix); err == nil {
		return q.GetReview(ctx, id)
	}
	rows, err := q.db.Query(ctx, `select `+reviewCols+` from reviews
		where id::text like $1 || '%' and ($2::uuid[] is null or repo_id = any($2)) order by created_at desc limit 2`, prefix, repos)
	if err != nil {
		return Review{}, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return Review{}, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return Review{}, err
	}
	switch len(out) {
	case 0:
		return Review{}, fmt.Errorf("review %s: %w", prefix, ErrNotFound)
	case 1:
		return out[0], nil
	}
	return Review{}, fmt.Errorf("review %s matches more than one review; give more of the id: %w", prefix, ErrAmbiguous)
}

// PendingReviewOf returns the workspace's pending review.
func (q Q) PendingReviewOf(ctx context.Context, workspace uuid.UUID) (Review, error) {
	r, err := scanReview(q.db.QueryRow(ctx, `select `+reviewCols+` from reviews where workspace_id = $1 and state = 'pending'`, workspace))
	return r, notFound(err, "pending review")
}

// ReviewFilter selects reviews. Zero fields do not filter.
type ReviewFilter struct {
	Repos    []uuid.UUID // nil: every repository
	EffortID uuid.UUID
	States   []ReviewState
	Limit    int
}

// ListReviews returns reviews, newest first.
func (q Q) ListReviews(ctx context.Context, f ReviewFilter) ([]Review, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	var effort *uuid.UUID
	if f.EffortID != uuid.Nil {
		effort = &f.EffortID
	}
	var states []string
	for _, s := range f.States {
		states = append(states, string(s))
	}
	rows, err := q.db.Query(ctx, `select `+reviewCols+` from reviews
		where ($1::uuid[] is null or repo_id = any($1)) and ($2::uuid is null or effort_id = $2)
		and ($3::text[] is null or state = any($3)) order by created_at desc, id limit $4`, f.Repos, effort, states, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideReview moves a pending review to approved or rejected. It reports
// false when the review was not pending any more.
func (q Q) DecideReview(ctx context.Context, id uuid.UUID, state ReviewState, by uuid.UUID, reason string, landed hash.Hash, at time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `update reviews set state = $2, decided_by = $3, decided_at = $4, reason = $5, landed = $6,
		conflicts = '{}', updated_at = $4 where id = $1 and state = 'pending'`, id, state, by, at, reason, landed)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetReviewConflicts records that approving the review conflicted; it stays
// pending.
func (q Q) SetReviewConflicts(ctx context.Context, id uuid.UUID, paths []string, by uuid.UUID, at time.Time) error {
	_, err := q.db.Exec(ctx, `update reviews set conflicts = $2, decided_by = $3, decided_at = $4, updated_at = $4
		where id = $1 and state = 'pending'`, id, nonNil(paths), by, at)
	return err
}

// WithdrawReviews withdraws the workspace's pending review, if any.
func (q Q) WithdrawReviews(ctx context.Context, workspace uuid.UUID) error {
	_, err := q.db.Exec(ctx, `update reviews set state = 'withdrawn', updated_at = now() where workspace_id = $1 and state = 'pending'`, workspace)
	return err
}
