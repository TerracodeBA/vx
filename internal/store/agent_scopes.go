package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AgentScope limits what an agent may change and how much (agent_scopes).
// The zero value has no limits; zero budgets mean no limit.
type AgentScope struct {
	AgentID        uuid.UUID
	Allow          []string // globs; empty: every path the agent's person may change
	Deny           []string // globs; win over Allow
	MaxLandsPerDay int
	MaxLines       int // changed lines (added + deleted) per land
	MaxFiles       int // changed files per land
	MaxWorkspaces  int // open workspaces at once
	UpdatedBy      *uuid.UUID
	UpdatedAt      time.Time
}

// Unlimited reports whether the scope limits nothing.
func (s AgentScope) Unlimited() bool {
	return len(s.Allow) == 0 && len(s.Deny) == 0 && s.MaxLandsPerDay == 0 && s.MaxLines == 0 && s.MaxFiles == 0 && s.MaxWorkspaces == 0
}

// AgentScope returns the scope of an agent; the zero value (with AgentID
// set) when none was ever set.
func (q Q) AgentScope(ctx context.Context, agentID uuid.UUID) (AgentScope, error) {
	s := AgentScope{AgentID: agentID}
	err := q.db.QueryRow(ctx, `select allow, deny, max_lands_per_day, max_lines, max_files, max_workspaces, updated_by, updated_at
		from agent_scopes where agent_id = $1`, agentID).
		Scan(&s.Allow, &s.Deny, &s.MaxLandsPerDay, &s.MaxLines, &s.MaxFiles, &s.MaxWorkspaces, &s.UpdatedBy, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentScope{AgentID: agentID}, nil
	}
	return s, err
}

// SetAgentScope stores an agent's scope; an unlimited one removes the row.
func (q Q) SetAgentScope(ctx context.Context, s AgentScope) error {
	if s.Unlimited() {
		_, err := q.db.Exec(ctx, `delete from agent_scopes where agent_id = $1`, s.AgentID)
		return err
	}
	if s.Allow == nil {
		s.Allow = []string{}
	}
	if s.Deny == nil {
		s.Deny = []string{}
	}
	_, err := q.db.Exec(ctx, `insert into agent_scopes
			(agent_id, allow, deny, max_lands_per_day, max_lines, max_files, max_workspaces, updated_by, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (agent_id) do update set allow = excluded.allow, deny = excluded.deny,
			max_lands_per_day = excluded.max_lands_per_day, max_lines = excluded.max_lines,
			max_files = excluded.max_files, max_workspaces = excluded.max_workspaces,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		s.AgentID, s.Allow, s.Deny, s.MaxLandsPerDay, s.MaxLines, s.MaxFiles, s.MaxWorkspaces, s.UpdatedBy, s.UpdatedAt)
	return err
}

// CountLands counts the changes a principal (by ref, "agent:alice/backend")
// landed in [from, to): distinct change ids of vx changesets it authored
// in an effort, so rebased copies of a change do not count again.
func (q Q) CountLands(ctx context.Context, ref string, from, to time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `select count(distinct change_id) from changesets
		where origin = 'vx' and author_principal = $1 and created_at >= $2 and created_at < $3`, ref, from, to).Scan(&n)
	return n, err
}

// LockPrincipal locks a principal's row until the transaction ends, so
// checks of its budgets and the writes they guard happen one at a time.
func (q Q) LockPrincipal(ctx context.Context, id uuid.UUID) error {
	var one int
	return notFound(q.db.QueryRow(ctx, `select 1 from principals where id = $1 for update`, id).Scan(&one), "principal")
}

// OpenWorkspacesOf lists the open workspaces a principal owns in open
// efforts, oldest first.
func (q Q) OpenWorkspacesOf(ctx context.Context, owner uuid.UUID) ([]Workspace, error) {
	rows, err := q.db.Query(ctx, `select `+prefixed("w", workspaceCols)+` from workspaces w
		join efforts e on e.id = w.effort_id
		where w.owner_id = $1 and w.state = 'open' and e.state = 'open'
		order by w.created_at`, owner)
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
