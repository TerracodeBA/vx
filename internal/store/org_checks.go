package store

// Organization checks (#47 part 3): versions of an organization's [[check]]
// tables, and the GitHub check runs vx keeps per check on a pull request.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OrgCheckVersion is one saved version of an organization's checks.
type OrgCheckVersion struct {
	OrgID      uuid.UUID
	Version    int
	Source     string          // the [[check]] tables as written
	Checks     json.RawMessage // the parsed definitions, a JSON list
	CreatedBy  *uuid.UUID
	CreatedRef string // "user:alice", kept when the person is removed
	CreatedAt  time.Time
}

const orgCheckCols = `org_id, version, source, checks, created_by, created_ref, created_at`

func scanOrgCheckVersion(row interface{ Scan(...any) error }) (OrgCheckVersion, error) {
	var v OrgCheckVersion
	var checks []byte
	err := row.Scan(&v.OrgID, &v.Version, &v.Source, &checks, &v.CreatedBy, &v.CreatedRef, &v.CreatedAt)
	v.Checks = checks
	return v, err
}

// AddOrgCheckVersion saves a new version of an organization's checks, one
// more than the newest. Two saves at once get different versions (the
// second waits for the first's lock on the organization).
func (q Q) AddOrgCheckVersion(ctx context.Context, v OrgCheckVersion) (OrgCheckVersion, error) {
	if _, err := q.db.Exec(ctx, `select 1 from orgs where id = $1 for update`, v.OrgID); err != nil {
		return OrgCheckVersion{}, err
	}
	if len(v.Checks) == 0 {
		v.Checks = json.RawMessage("[]")
	}
	return scanOrgCheckVersion(q.db.QueryRow(ctx, `insert into org_check_versions (org_id, version, source, checks, created_by, created_ref)
		values ($1, coalesce((select max(version) from org_check_versions where org_id = $1), 0) + 1, $2, $3, $4, $5)
		returning `+orgCheckCols, v.OrgID, v.Source, []byte(v.Checks), v.CreatedBy, v.CreatedRef))
}

// OrgChecks returns the newest version of an organization's checks; ok is
// false when it never defined any.
func (q Q) OrgChecks(ctx context.Context, org uuid.UUID) (OrgCheckVersion, bool, error) {
	v, err := scanOrgCheckVersion(q.db.QueryRow(ctx, `select `+orgCheckCols+` from org_check_versions where org_id = $1 order by version desc limit 1`, org))
	if errors.Is(err, pgx.ErrNoRows) {
		return OrgCheckVersion{}, false, nil
	}
	return v, err == nil, err
}

// OrgCheckHistory lists an organization's versions, newest first, at most
// limit (0: all).
func (q Q) OrgCheckHistory(ctx context.Context, org uuid.UUID, limit int) ([]OrgCheckVersion, error) {
	sql := `select ` + orgCheckCols + ` from org_check_versions where org_id = $1 order by version desc`
	args := []any{org}
	if limit > 0 {
		sql += ` limit $2`
		args = append(args, limit)
	}
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgCheckVersion
	for rows.Next() {
		v, err := scanOrgCheckVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GitHubCheckRunRow is the GitHub check run vx keeps for one check on one
// pull request head.
type GitHubCheckRunRow struct {
	ShipmentID uuid.UUID
	CheckName  string
	HeadSHA    string
	GitHubID   int64
	State      string // what was last published
	UpdatedAt  time.Time
}

// GitHubCheckRuns lists the check runs of a shipment's head.
func (q Q) GitHubCheckRuns(ctx context.Context, shipment uuid.UUID, headSHA string) ([]GitHubCheckRunRow, error) {
	rows, err := q.db.Query(ctx, `select shipment_id, check_name, head_sha, github_id, state, updated_at
		from github_check_runs where shipment_id = $1 and head_sha = $2 order by check_name`, shipment, headSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitHubCheckRunRow
	for rows.Next() {
		var r GitHubCheckRunRow
		if err := rows.Scan(&r.ShipmentID, &r.CheckName, &r.HeadSHA, &r.GitHubID, &r.State, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveGitHubCheckRun records a check run vx created or updated.
func (q Q) SaveGitHubCheckRun(ctx context.Context, r GitHubCheckRunRow) error {
	_, err := q.db.Exec(ctx, `insert into github_check_runs (shipment_id, check_name, head_sha, github_id, state)
		values ($1, $2, $3, $4, $5)
		on conflict (shipment_id, check_name, head_sha) do update set github_id = excluded.github_id, state = excluded.state, updated_at = now()`,
		r.ShipmentID, r.CheckName, r.HeadSHA, r.GitHubID, r.State)
	return err
}
