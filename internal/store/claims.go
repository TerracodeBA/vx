package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Claim is a stored claim. RepoName and EffortSlug are read from the
// claim's repo and effort. A claim is active while ReleasedAt is nil and
// either ExpiresAt is after now or the workspace it is tied to is open: a
// workspace's claims last as long as the workspace.
type Claim struct {
	ID          uuid.UUID
	RepoID      uuid.UUID
	EffortID    uuid.UUID
	OwnerID     uuid.UUID
	WorkspaceID *uuid.UUID
	Targets     []string
	Reason      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ReleasedAt  *time.Time
	RepoName    string
	EffortSlug  string
	// WorkspaceOpen is read from the workspace the claim is tied to.
	WorkspaceOpen bool
	// MovingTo is where the claimed code is being moved (vx claim
	// --moving pkg.Func --to path); empty for other claims.
	MovingTo string
}

// Active reports whether the claim is neither released nor expired at now.
func (c Claim) Active(now time.Time) bool {
	return c.ReleasedAt == nil && (c.ExpiresAt.After(now) || c.WorkspaceOpen)
}

// claimWorkspaceOpen is true for a claim tied to an open workspace.
const claimWorkspaceOpen = `exists (select 1 from workspaces w where w.id = c.workspace_id and w.state = 'open')`

const claimCols = `c.id, c.repo_id, c.effort_id, c.owner_id, c.workspace_id, c.targets, c.reason,
	c.created_at, c.expires_at, c.released_at, r.name, e.slug, ` + claimWorkspaceOpen + `, c.moving_to`

const claimFrom = ` from claims c join efforts e on e.id = c.effort_id join repos r on r.id = c.repo_id `

func scanClaim(row interface{ Scan(...any) error }) (Claim, error) {
	var c Claim
	err := row.Scan(&c.ID, &c.RepoID, &c.EffortID, &c.OwnerID, &c.WorkspaceID, &c.Targets, &c.Reason,
		&c.CreatedAt, &c.ExpiresAt, &c.ReleasedAt, &c.RepoName, &c.EffortSlug, &c.WorkspaceOpen, &c.MovingTo)
	if c.Targets == nil {
		c.Targets = []string{}
	}
	return c, err
}

func (q Q) queryClaims(ctx context.Context, sql string, args ...any) ([]Claim, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertClaim stores a claim. A zero ID gets a new one; a zero CreatedAt
// means now. The repo is taken from the effort.
func (q Q) InsertClaim(ctx context.Context, c Claim) (Claim, error) {
	if c.ID == uuid.Nil {
		c.ID = NewID()
	}
	if c.Targets == nil {
		c.Targets = []string{}
	}
	var created *time.Time
	if !c.CreatedAt.IsZero() {
		created = &c.CreatedAt
	}
	tag, err := q.db.Exec(ctx, `insert into claims (id, repo_id, effort_id, owner_id, workspace_id, targets, reason, created_at, expires_at, moving_to)
		select $1::uuid, e.repo_id, e.id, $3::uuid, $4::uuid, $5::text[], $6::text, coalesce($7::timestamptz, now()), $8::timestamptz, $9::text
		from efforts e where e.id = $2::uuid`,
		c.ID, c.EffortID, c.OwnerID, c.WorkspaceID, c.Targets, c.Reason, created, c.ExpiresAt, c.MovingTo)
	if err != nil {
		return Claim{}, err
	}
	if tag.RowsAffected() == 0 {
		return Claim{}, fmt.Errorf("effort: %w", ErrNotFound)
	}
	return q.ClaimByID(ctx, c.ID)
}

// ClaimByID loads a claim, active or not.
func (q Q) ClaimByID(ctx context.Context, id uuid.UUID) (Claim, error) {
	c, err := scanClaim(q.db.QueryRow(ctx, `select `+claimCols+claimFrom+`where c.id = $1`, id))
	return c, notFound(err, "claim "+id.String())
}

// ClaimByPrefix finds the one claim whose id starts with prefix (lowercase
// hex and dashes). It returns ErrNotFound or ErrAmbiguous otherwise.
func (q Q) ClaimByPrefix(ctx context.Context, prefix string) (Claim, error) {
	prefix = strings.ToLower(prefix)
	for _, r := range prefix {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == '-') {
			return Claim{}, fmt.Errorf("claim %q: %w", prefix, ErrNotFound)
		}
	}
	cs, err := q.queryClaims(ctx, `select `+claimCols+claimFrom+`where c.id::text like $1 || '%' limit 2`, prefix)
	if err != nil {
		return Claim{}, err
	}
	switch len(cs) {
	case 0:
		return Claim{}, fmt.Errorf("claim %s: %w", prefix, ErrNotFound)
	case 1:
		return cs[0], nil
	}
	return Claim{}, fmt.Errorf("claim prefix %s: %w", prefix, ErrAmbiguous)
}

// ActiveClaims lists an effort's claims active at now, oldest first.
func (q Q) ActiveClaims(ctx context.Context, effortID uuid.UUID, now time.Time) ([]Claim, error) {
	return q.queryClaims(ctx, `select `+claimCols+claimFrom+`where c.effort_id = $1 and c.released_at is null
		and (c.expires_at > $2 or `+claimWorkspaceOpen+`)
		order by c.created_at, c.seq`, effortID, now)
}

// ReleaseClaim releases one claim if it is still active at now and returns
// it; ok is false when it was already released or expired.
func (q Q) ReleaseClaim(ctx context.Context, id uuid.UUID, now time.Time) (Claim, bool, error) {
	tag, err := q.db.Exec(ctx, `update claims c set released_at = $2 where id = $1 and released_at is null and (expires_at > $2 or `+claimWorkspaceOpen+`)`, id, now)
	if err != nil {
		return Claim{}, false, err
	}
	c, err := q.ClaimByID(ctx, id)
	return c, tag.RowsAffected() == 1, err
}

// ReleaseWorkspaceClaims releases the claims tied to a workspace that are
// not released yet (they lasted as long as the workspace) and returns them.
// A non-nil owner releases only that principal's claims.
func (q Q) ReleaseWorkspaceClaims(ctx context.Context, workspaceID uuid.UUID, owner *uuid.UUID, now time.Time) ([]Claim, error) {
	return q.queryClaims(ctx, `with released as (
			update claims set released_at = $3
			where workspace_id = $1 and ($2::uuid is null or owner_id = $2) and released_at is null
			returning *
		)
		select `+claimCols+` from released c join efforts e on e.id = c.effort_id join repos r on r.id = c.repo_id
		order by c.created_at, c.seq`, workspaceID, owner, now)
}

// KeepMovingClaims detaches the active moving claims of a workspace that
// lands from it, so they outlive the land, and keeps them until at least
// until.
func (q Q) KeepMovingClaims(ctx context.Context, workspaceID uuid.UUID, until time.Time) error {
	_, err := q.db.Exec(ctx, `update claims set workspace_id = null, expires_at = greatest(expires_at, $2)
		where workspace_id = $1 and moving_to <> '' and released_at is null`, workspaceID, until)
	return err
}

// workspaceLastActive is when workspace w last did something: synced,
// landed or reported its files, or its owner used a token.
const workspaceLastActive = `greatest(w.created_at, w.active_at, w.changed_paths_at,
	(select max(t.last_used_at) from tokens t where t.principal_id = w.owner_id))`

// LapseIdleClaims releases, at now, the claims of an effort that are alive
// only because their workspace is open (past their own expiry) while that
// workspace has done nothing since idleSince, and returns them.
func (q Q) LapseIdleClaims(ctx context.Context, effortID uuid.UUID, now, idleSince time.Time) ([]Claim, error) {
	return q.queryClaims(ctx, `with lapsed as (
			update claims c set released_at = $2
			where c.effort_id = $1 and c.released_at is null and c.expires_at <= $2
			and exists (select 1 from workspaces w where w.id = c.workspace_id and w.state = 'open'
				and `+workspaceLastActive+` < $3)
			returning *
		)
		select `+strings.ReplaceAll(claimCols, claimWorkspaceOpen, "false")+` from lapsed c join efforts e on e.id = c.effort_id join repos r on r.id = c.repo_id
		order by c.created_at, c.seq`, effortID, now, idleSince)
}
