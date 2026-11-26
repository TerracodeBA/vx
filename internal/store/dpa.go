package store

// The data processing agreement (#41): acceptances per organization and
// the accepted texts.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OrgAgreement is one acceptance of a version of the data processing
// agreement for an organization.
type OrgAgreement struct {
	ID            uuid.UUID
	OrgID         uuid.UUID
	VersionID     string
	SHA256        string // of the text as accepted, hex
	AcceptedBy    *uuid.UUID
	AcceptedByRef string // "user:alice", kept when the person is merged or removed
	AcceptedAt    time.Time
	IP            string
}

const orgAgreementCols = `id, org_id, version_id, sha256, accepted_by, accepted_by_ref, accepted_at, ip`

func scanOrgAgreement(row interface{ Scan(...any) error }) (OrgAgreement, error) {
	var a OrgAgreement
	err := row.Scan(&a.ID, &a.OrgID, &a.VersionID, &a.SHA256, &a.AcceptedBy, &a.AcceptedByRef, &a.AcceptedAt, &a.IP)
	return a, err
}

// AcceptOrgAgreement records an acceptance and keeps the text it was for.
// Accepting a version the organization accepted before returns that
// acceptance unchanged, with created false.
func (q Q) AcceptOrgAgreement(ctx context.Context, a OrgAgreement, text []byte) (out OrgAgreement, created bool, err error) {
	if _, err := q.db.Exec(ctx, `insert into dpa_texts (sha256, body) values ($1, $2) on conflict (sha256) do nothing`, a.SHA256, text); err != nil {
		return OrgAgreement{}, false, err
	}
	out, err = scanOrgAgreement(q.db.QueryRow(ctx, `insert into org_dpa_acceptances (org_id, version_id, sha256, accepted_by, accepted_by_ref, accepted_at, ip)
		values ($1, $2, $3, $4, $5, $6, $7) on conflict (org_id, version_id) do nothing returning `+orgAgreementCols,
		a.OrgID, a.VersionID, a.SHA256, a.AcceptedBy, a.AcceptedByRef, a.AcceptedAt, a.IP))
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OrgAgreement{}, false, err
	}
	out, err = scanOrgAgreement(q.db.QueryRow(ctx, `select `+orgAgreementCols+` from org_dpa_acceptances where org_id = $1 and version_id = $2`, a.OrgID, a.VersionID))
	return out, false, err
}

// OrgAgreements lists an organization's acceptances, newest first.
func (q Q) OrgAgreements(ctx context.Context, org uuid.UUID) ([]OrgAgreement, error) {
	return q.listOrgAgreements(ctx, `select `+orgAgreementCols+` from org_dpa_acceptances where org_id = $1 order by accepted_at desc, id`, org)
}

// OrgAgreementsOf lists the acceptances of several organizations, newest
// first, for the banner and notifications.
func (q Q) OrgAgreementsOf(ctx context.Context, orgs []uuid.UUID) ([]OrgAgreement, error) {
	if len(orgs) == 0 {
		return nil, nil
	}
	return q.listOrgAgreements(ctx, `select `+orgAgreementCols+` from org_dpa_acceptances where org_id = any($1) order by accepted_at desc, id`, orgs)
}

// OrgAgreementByID returns one acceptance of an organization.
func (q Q) OrgAgreementByID(ctx context.Context, org, id uuid.UUID) (OrgAgreement, error) {
	a, err := scanOrgAgreement(q.db.QueryRow(ctx, `select `+orgAgreementCols+` from org_dpa_acceptances where org_id = $1 and id = $2`, org, id))
	return a, notFound(err, "acceptance")
}

func (q Q) listOrgAgreements(ctx context.Context, sql string, arg any) ([]OrgAgreement, error) {
	rows, err := q.db.Query(ctx, sql, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgAgreement
	for rows.Next() {
		a, err := scanOrgAgreement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgreementText returns an accepted text by its SHA-256.
func (q Q) AgreementText(ctx context.Context, sha256 string) ([]byte, error) {
	var body []byte
	err := q.db.QueryRow(ctx, `select body from dpa_texts where sha256 = $1`, sha256).Scan(&body)
	return body, notFound(err, "agreement text")
}

// AcceptedAgreementHashes returns, for every version id anyone accepted,
// the hashes of the texts accepted under it (one, unless the operator
// changed a published text).
func (q Q) AcceptedAgreementHashes(ctx context.Context) (map[string][]string, error) {
	rows, err := q.db.Query(ctx, `select distinct version_id, sha256 from org_dpa_acceptances order by version_id, sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var v, h string
		if err := rows.Scan(&v, &h); err != nil {
			return nil, err
		}
		out[v] = append(out[v], h)
	}
	return out, rows.Err()
}

// OrgsWithRepos reports which of orgs have at least one repository.
func (q Q) OrgsWithRepos(ctx context.Context, orgs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(orgs) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `select distinct org_id from repos where org_id = any($1)`, orgs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
