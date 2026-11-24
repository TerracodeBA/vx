package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OrgQuota is an organization's override of the server's quotas. A nil
// field keeps the server's default; 0 means no limit.
type OrgQuota struct {
	OrgID       uuid.UUID
	Repos       *int64
	StoredBytes *int64
	ImportBytes *int64
	PushBytes   *int64
	OpenEfforts *int64
	UpdatedBy   *uuid.UUID
	UpdatedAt   time.Time
}

// Empty reports whether the override changes nothing.
func (o OrgQuota) Empty() bool {
	return o.Repos == nil && o.StoredBytes == nil && o.ImportBytes == nil && o.PushBytes == nil && o.OpenEfforts == nil
}

// GetOrgQuota returns an organization's override; one without a row has
// every field nil.
func (q Q) GetOrgQuota(ctx context.Context, orgID uuid.UUID) (OrgQuota, error) {
	o := OrgQuota{OrgID: orgID}
	err := q.db.QueryRow(ctx, `select repos, stored_bytes, import_bytes, push_bytes, open_efforts, updated_by, updated_at
		from org_quotas where org_id = $1`, orgID).
		Scan(&o.Repos, &o.StoredBytes, &o.ImportBytes, &o.PushBytes, &o.OpenEfforts, &o.UpdatedBy, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, nil
	}
	return o, err
}

// SetOrgQuota stores an override; an empty one deletes the row.
func (q Q) SetOrgQuota(ctx context.Context, o OrgQuota) error {
	if o.Empty() {
		_, err := q.db.Exec(ctx, `delete from org_quotas where org_id = $1`, o.OrgID)
		return err
	}
	_, err := q.db.Exec(ctx, `insert into org_quotas (org_id, repos, stored_bytes, import_bytes, push_bytes, open_efforts, updated_by, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, now())
		on conflict (org_id) do update set repos = $2, stored_bytes = $3, import_bytes = $4, push_bytes = $5,
			open_efforts = $6, updated_by = $7, updated_at = now()`,
		o.OrgID, o.Repos, o.StoredBytes, o.ImportBytes, o.PushBytes, o.OpenEfforts, o.UpdatedBy)
	return err
}

// OrgUsage counts what an organization uses: its repositories and its
// efforts that are open or shipping. Counts only, never names.
func (q Q) OrgUsage(ctx context.Context, orgID uuid.UUID) (repos, openEfforts int64, err error) {
	err = q.db.QueryRow(ctx, `select
		(select count(*) from repos where org_id = $1),
		(select count(*) from efforts e join repos r on r.id = e.repo_id
			where r.org_id = $1 and e.state in ('open', 'shipping'))`, orgID).Scan(&repos, &openEfforts)
	return repos, openEfforts, err
}

// QuotaOrg looks an organization up by id, for quota checks on its
// repositories.
func (q Q) QuotaOrg(ctx context.Context, id uuid.UUID) (Org, error) {
	o, err := scanOrg(q.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.id = $1`, id))
	return o, notFound(err, "organization "+id.String())
}
