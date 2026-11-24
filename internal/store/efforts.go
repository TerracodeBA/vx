package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// EffortState is the lifecycle state of an effort.
type EffortState string

const (
	EffortOpen      EffortState = "open"
	EffortShipping  EffortState = "shipping"
	EffortMerged    EffortState = "merged"
	EffortAbandoned EffortState = "abandoned"
)

// Effort is a shared line of work that agents land changes into.
type Effort struct {
	ID         uuid.UUID
	RepoID     uuid.UUID
	Slug       string
	Name       string
	Goal       string
	BaseBranch string
	Base       hash.Hash // upstream changeset the effort sits on
	Head       hash.Hash
	Version    int64
	State      EffortState
	OwnerID    uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const effortCols = `id, repo_id, slug, name, goal, base_branch, base, head, version, state, owner_id, created_at, updated_at`

func scanEffort(row interface{ Scan(...any) error }) (Effort, error) {
	var e Effort
	err := row.Scan(&e.ID, &e.RepoID, &e.Slug, &e.Name, &e.Goal, &e.BaseBranch, &e.Base, &e.Head,
		&e.Version, &e.State, &e.OwnerID, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Slug length limits: slugs longer than slugMax are cut at the last word
// boundary at or before slugMax, unless that would leave fewer than slugMin
// characters, in which case they are cut at slugMax.
const (
	slugMax = 40
	slugMin = 20
)

// Slugify turns an effort name into a short URL-safe slug.
func Slugify(name string) string {
	s := slugStrip.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > slugMax {
		// s[slugMax] == '-' means s[:slugMax] already ends on a whole word.
		if i := strings.LastIndexByte(s[:slugMax+1], '-'); i >= slugMin {
			s = s[:i]
		} else {
			s = s[:slugMax]
		}
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		s = "effort"
	}
	return s
}

// CreateEffort inserts an effort with a unique slug derived from its name.
func (q Q) CreateEffort(ctx context.Context, e Effort) (Effort, error) {
	if strings.TrimSpace(e.Name) == "" {
		return Effort{}, fmt.Errorf("effort needs a name")
	}
	base := e.Slug
	if base == "" {
		base = Slugify(e.Name)
	}
	for n := 1; n < 100; n++ {
		slug := base
		if n > 1 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		row := q.db.QueryRow(ctx, `insert into efforts (repo_id, slug, name, goal, base_branch, base, head, owner_id)
			values ($1, $2, $3, $4, $5, $6, $6, $7)
			on conflict (repo_id, slug) do nothing
			returning `+effortCols, e.RepoID, slug, e.Name, e.Goal, e.BaseBranch, e.Base, e.OwnerID)
		out, err := scanEffort(row)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Effort{}, err
		}
		// No row: the slug is taken, try the next one.
	}
	return Effort{}, fmt.Errorf("could not find a free slug for %q", e.Name)
}

// GetEffort finds an effort by repo and slug.
func (q Q) GetEffort(ctx context.Context, repoID uuid.UUID, slug string) (Effort, error) {
	e, err := scanEffort(q.db.QueryRow(ctx, `select `+effortCols+` from efforts where repo_id = $1 and slug = $2`, repoID, slug))
	return e, notFound(err, "effort "+slug)
}

// EffortByID finds an effort by id.
func (q Q) EffortByID(ctx context.Context, id uuid.UUID) (Effort, error) {
	e, err := scanEffort(q.db.QueryRow(ctx, `select `+effortCols+` from efforts where id = $1`, id))
	return e, notFound(err, "effort")
}

// ListEfforts lists a repo's efforts, newest first. An empty state lists all.
func (q Q) ListEfforts(ctx context.Context, repoID uuid.UUID, state EffortState) ([]Effort, error) {
	rows, err := q.db.Query(ctx, `select `+effortCols+` from efforts
		where repo_id = $1 and ($2 = '' or state = $2) order by created_at desc`, repoID, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Effort
	for rows.Next() {
		e, err := scanEffort(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AdvanceEffort moves the effort head with a compare-and-swap on version.
// It reports false when another writer moved the head first.
func (q Q) AdvanceEffort(ctx context.Context, id uuid.UUID, seenVersion int64, newHead hash.Hash) (bool, error) {
	tag, err := q.db.Exec(ctx, `update efforts set head = $1, version = version + 1, updated_at = now()
		where id = $2 and version = $3 and state = 'open'`, newHead, id, seenVersion)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RebaseEffort moves the effort onto a new base and head, with the same
// compare-and-swap as AdvanceEffort.
func (q Q) RebaseEffort(ctx context.Context, id uuid.UUID, seenVersion int64, newBase, newHead hash.Hash) (bool, error) {
	tag, err := q.db.Exec(ctx, `update efforts set base = $1, head = $2, version = version + 1, updated_at = now()
		where id = $3 and version = $4 and state = 'open'`, newBase, newHead, id, seenVersion)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetEffortState changes the lifecycle state.
func (q Q) SetEffortState(ctx context.Context, id uuid.UUID, state EffortState) error {
	_, err := q.db.Exec(ctx, `update efforts set state = $1, updated_at = now() where id = $2`, state, id)
	return err
}

// WorkspaceState is the lifecycle state of a workspace.
type WorkspaceState string

const (
	WorkspaceOpen      WorkspaceState = "open"
	WorkspaceLanded    WorkspaceState = "landed"
	WorkspaceAbandoned WorkspaceState = "abandoned"
)

// Workspace is one task's draft inside an effort.
type Workspace struct {
	ID        uuid.UUID
	EffortID  uuid.UUID
	OwnerID   uuid.UUID
	Intent    string
	ChangeID  string
	Base      hash.Hash
	State     WorkspaceState
	LandedAs  hash.Hash
	CreatedAt time.Time
	UpdatedAt time.Time
}

const workspaceCols = `id, effort_id, owner_id, intent, change_id, base, state, landed_as, created_at, updated_at`

func scanWorkspace(row interface{ Scan(...any) error }) (Workspace, error) {
	var w Workspace
	err := row.Scan(&w.ID, &w.EffortID, &w.OwnerID, &w.Intent, &w.ChangeID, &w.Base, &w.State, &w.LandedAs, &w.CreatedAt, &w.UpdatedAt)
	return w, err
}

// CreateWorkspace inserts an open workspace.
func (q Q) CreateWorkspace(ctx context.Context, w Workspace) (Workspace, error) {
	if strings.TrimSpace(w.Intent) == "" {
		return Workspace{}, fmt.Errorf("workspace needs an intent")
	}
	if w.ChangeID == "" {
		w.ChangeID = object.NewChangeID()
	}
	return scanWorkspace(q.db.QueryRow(ctx, `insert into workspaces (effort_id, owner_id, intent, change_id, base)
		values ($1, $2, $3, $4, $5) returning `+workspaceCols, w.EffortID, w.OwnerID, w.Intent, w.ChangeID, w.Base))
}

// GetWorkspace loads a workspace.
func (q Q) GetWorkspace(ctx context.Context, id uuid.UUID) (Workspace, error) {
	w, err := scanWorkspace(q.db.QueryRow(ctx, `select `+workspaceCols+` from workspaces where id = $1`, id))
	return w, notFound(err, "workspace")
}

// GetWorkspaceForUpdate loads a workspace and locks its row until the
// transaction ends, so two lands of the same workspace cannot race.
func (q Q) GetWorkspaceForUpdate(ctx context.Context, id uuid.UUID) (Workspace, error) {
	w, err := scanWorkspace(q.db.QueryRow(ctx, `select `+workspaceCols+` from workspaces where id = $1 for update`, id))
	return w, notFound(err, "workspace")
}

// ListWorkspaces lists an effort's workspaces. An empty state lists all.
func (q Q) ListWorkspaces(ctx context.Context, effortID uuid.UUID, state WorkspaceState) ([]Workspace, error) {
	rows, err := q.db.Query(ctx, `select `+workspaceCols+` from workspaces
		where effort_id = $1 and ($2 = '' or state = $2) order by created_at`, effortID, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWorkspaceBase records the snapshot the workspace's files are based on.
func (q Q) SetWorkspaceBase(ctx context.Context, id uuid.UUID, base hash.Hash) error {
	_, err := q.db.Exec(ctx, `update workspaces set base = $1, updated_at = now() where id = $2 and state = 'open'`, base, id)
	return err
}

// FinishWorkspace marks a workspace landed (with its changeset) or abandoned.
func (q Q) FinishWorkspace(ctx context.Context, id uuid.UUID, state WorkspaceState, landedAs hash.Hash) error {
	_, err := q.db.Exec(ctx, `update workspaces set state = $1, landed_as = $2, updated_at = now() where id = $3`, state, landedAs, id)
	return err
}

// Op is one entry in the operation log.
type Op struct {
	ID      int64
	RepoID  *uuid.UUID
	ActorID *uuid.UUID
	Op      string
	Target  string
	Before  json.RawMessage
	After   json.RawMessage
	At      time.Time
}

// LogOp appends to the operation log. before and after are marshaled to JSON.
func (q Q) LogOp(ctx context.Context, repoID, actorID *uuid.UUID, op, target string, before, after any) error {
	var b, a []byte
	var err error
	if before != nil {
		if b, err = json.Marshal(before); err != nil {
			return err
		}
	}
	if after != nil {
		if a, err = json.Marshal(after); err != nil {
			return err
		}
	}
	_, err = q.db.Exec(ctx, `insert into op_log (repo_id, actor_id, op, target, before, after) values ($1, $2, $3, $4, $5, $6)`,
		repoID, actorID, op, target, b, a)
	return err
}

// ListOps returns a repo's newest operations first.
func (q Q) ListOps(ctx context.Context, repoID uuid.UUID, limit int) ([]Op, error) {
	rows, err := q.db.Query(ctx, `select id, repo_id, actor_id, op, target, before, after, at
		from op_log where repo_id = $1 order by id desc limit $2`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		var o Op
		if err := rows.Scan(&o.ID, &o.RepoID, &o.ActorID, &o.Op, &o.Target, &o.Before, &o.After, &o.At); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
