package store

// Support grants and server notices (#33): a repository's admins give a
// superadmin a role on that repository until a time; the server records
// one-time upgrade notices.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SupportGrant is a time-limited role a repository's admins gave a
// superadmin on that repository.
type SupportGrant struct {
	RepoID    uuid.UUID
	Person    Principal
	Role      string
	ExpiresAt time.Time
	Reason    string
	GrantedBy *Principal // nil when that person is gone
	CreatedAt time.Time
}

// SetSupportGrant gives person role on repo until expires, replacing an
// earlier support grant to them. It returns the previous grant, if any.
func (q Q) SetSupportGrant(ctx context.Context, repo, person uuid.UUID, role string, expires time.Time, reason string, by *uuid.UUID) (*SupportGrant, error) {
	prev, err := q.supportGrant(ctx, repo, person)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	_, err = q.db.Exec(ctx, `insert into support_grants (repo_id, person_id, role, expires_at, reason, granted_by)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (repo_id, person_id) do update set role = excluded.role, expires_at = excluded.expires_at,
			reason = excluded.reason, granted_by = excluded.granted_by, created_at = now()`,
		repo, person, role, expires, reason, by)
	if err != nil {
		return nil, err
	}
	return prev, nil
}

// DeleteSupportGrant removes person's support grant on repo and returns it
// (ErrNotFound when there is none).
func (q Q) DeleteSupportGrant(ctx context.Context, repo, person uuid.UUID) (SupportGrant, error) {
	g, err := q.supportGrant(ctx, repo, person)
	if err != nil {
		return SupportGrant{}, err
	}
	if _, err := q.db.Exec(ctx, `delete from support_grants where repo_id = $1 and person_id = $2`, repo, person); err != nil {
		return SupportGrant{}, err
	}
	return *g, nil
}

const supportSelect = `select sg.repo_id, sg.role, sg.expires_at, sg.reason, sg.created_at, p.id, p.kind, p.handle, p.parent_id, p.display_name, p.email, p.site_admin, p.created_at,
	b.id, b.kind, b.handle, b.parent_id, b.display_name, b.email, b.site_admin, b.created_at
	from support_grants sg join principals p on p.id = sg.person_id left join principals b on b.id = sg.granted_by`

func scanSupport(row pgx.Row) (SupportGrant, error) {
	var g SupportGrant
	p := &g.Person
	var bid, bparent *uuid.UUID
	var bkind, bhandle, bdisplay, bemail *string
	var bsite *bool
	var bcreated *time.Time
	err := row.Scan(&g.RepoID, &g.Role, &g.ExpiresAt, &g.Reason, &g.CreatedAt,
		&p.ID, &p.Kind, &p.Handle, &p.ParentID, &p.DisplayName, &p.Email, &p.SiteAdmin, &p.CreatedAt,
		&bid, &bkind, &bhandle, &bparent, &bdisplay, &bemail, &bsite, &bcreated)
	if err != nil {
		return g, err
	}
	if bid != nil {
		g.GrantedBy = &Principal{ID: *bid, Kind: PrincipalKind(*bkind), Handle: *bhandle, ParentID: bparent,
			DisplayName: *bdisplay, Email: *bemail, SiteAdmin: *bsite, CreatedAt: *bcreated}
	}
	return g, nil
}

func (q Q) supportGrant(ctx context.Context, repo, person uuid.UUID) (*SupportGrant, error) {
	g, err := scanSupport(q.db.QueryRow(ctx, supportSelect+` where sg.repo_id = $1 and sg.person_id = $2`, repo, person))
	if err != nil {
		return nil, notFound(err, "support grant")
	}
	return &g, nil
}

// ListSupportGrants returns repo's support grants unexpired at at, by
// handle. Grants to people who are no longer superadmins are left out:
// they give no role.
func (q Q) ListSupportGrants(ctx context.Context, repo uuid.UUID, at time.Time) ([]SupportGrant, error) {
	rows, err := q.db.Query(ctx, supportSelect+` where sg.repo_id = $1 and sg.expires_at > $2
		and p.site_admin and p.kind = 'human' order by p.handle`, repo, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupportGrant
	for rows.Next() {
		g, err := scanSupport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ServerNotice is a one-time notice about an upgrade.
type ServerNotice struct {
	Key        string
	CreatedAt  time.Time
	LoggedAt   *time.Time
	ResolvedAt *time.Time
	ResolvedBy *uuid.UUID
}

// GetServerNotice reads a notice (ErrNotFound when the server never had it).
func (q Q) GetServerNotice(ctx context.Context, key string) (ServerNotice, error) {
	var n ServerNotice
	err := q.db.QueryRow(ctx, `select key, created_at, logged_at, resolved_at, resolved_by from server_notices where key = $1`, key).
		Scan(&n.Key, &n.CreatedAt, &n.LoggedAt, &n.ResolvedAt, &n.ResolvedBy)
	return n, notFound(err, "server notice "+key)
}

// MarkServerNoticeLogged records that the server logged a notice. It
// reports whether this call was the first to do so.
func (q Q) MarkServerNoticeLogged(ctx context.Context, key string) (bool, error) {
	tag, err := q.db.Exec(ctx, `update server_notices set logged_at = now() where key = $1 and logged_at is null`, key)
	return err == nil && tag.RowsAffected() == 1, err
}

// ResolveServerNotice records that by reviewed a notice. It reports whether
// the notice was still open.
func (q Q) ResolveServerNotice(ctx context.Context, key string, by uuid.UUID) (bool, error) {
	tag, err := q.db.Exec(ctx, `update server_notices set resolved_at = now(), resolved_by = $2 where key = $1 and resolved_at is null`, key, by)
	return err == nil && tag.RowsAffected() == 1, err
}

// AddServerNotice opens a notice (tests and future upgrades); an existing
// one is left as it is.
func (q Q) AddServerNotice(ctx context.Context, key string) error {
	_, err := q.db.Exec(ctx, `insert into server_notices (key) values ($1) on conflict do nothing`, key)
	return err
}
