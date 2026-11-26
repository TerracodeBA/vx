package store

// Single sign-on (OIDC) per organization, people's identities at identity
// providers, which tokens were issued through an organization's sign-on,
// and used nonces.

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OrgSSO is an organization's identity provider configuration.
type OrgSSO struct {
	OrgID           uuid.UUID
	Issuer          string
	ClientID        string
	ClientSecretEnc []byte
	AllowedDomains  []string
	RequireSSO      bool
	UpdatedBy       *uuid.UUID
	UpdatedAt       time.Time
}

const orgSSOCols = `org_id, issuer, client_id, client_secret_enc, allowed_domains, require_sso, updated_by, updated_at`

func scanOrgSSO(row interface{ Scan(...any) error }) (OrgSSO, error) {
	var c OrgSSO
	err := row.Scan(&c.OrgID, &c.Issuer, &c.ClientID, &c.ClientSecretEnc, &c.AllowedDomains, &c.RequireSSO, &c.UpdatedBy, &c.UpdatedAt)
	return c, err
}

// OrgSSOOf returns an organization's configuration; ok is false without one.
func (q Q) OrgSSOOf(ctx context.Context, org uuid.UUID) (OrgSSO, bool, error) {
	c, err := scanOrgSSO(q.db.QueryRow(ctx, `select `+orgSSOCols+` from org_sso where org_id = $1`, org))
	if errors.Is(err, pgx.ErrNoRows) {
		return OrgSSO{}, false, nil
	}
	return c, err == nil, err
}

// PutOrgSSO creates or replaces an organization's configuration.
func (q Q) PutOrgSSO(ctx context.Context, c OrgSSO) (OrgSSO, error) {
	if c.AllowedDomains == nil {
		c.AllowedDomains = []string{}
	}
	return scanOrgSSO(q.db.QueryRow(ctx, `insert into org_sso (org_id, issuer, client_id, client_secret_enc, allowed_domains, require_sso, updated_by)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (org_id) do update set issuer = excluded.issuer, client_id = excluded.client_id,
			client_secret_enc = excluded.client_secret_enc, allowed_domains = excluded.allowed_domains,
			require_sso = excluded.require_sso, updated_by = excluded.updated_by, updated_at = now()
		returning `+orgSSOCols, c.OrgID, c.Issuer, c.ClientID, c.ClientSecretEnc, c.AllowedDomains, c.RequireSSO, c.UpdatedBy))
}

// DeleteOrgSSO removes an organization's configuration and the sign-on
// marks of its tokens.
func (q Q) DeleteOrgSSO(ctx context.Context, org uuid.UUID) (bool, error) {
	if _, err := q.db.Exec(ctx, `delete from token_sso where org_id = $1`, org); err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `delete from org_sso where org_id = $1`, org)
	return tag.RowsAffected() == 1, err
}

// ForgetOrgSSOProofs drops the sign-on marks of an organization's tokens,
// for example after its provider changed.
func (q Q) ForgetOrgSSOProofs(ctx context.Context, org uuid.UUID) error {
	_, err := q.db.Exec(ctx, `delete from token_sso where org_id = $1`, org)
	return err
}

// OrgsBySSODomain lists the organizations whose sign-on allows email
// domain, with their handles.
func (q Q) OrgsBySSODomain(ctx context.Context, domain string) ([]Org, error) {
	rows, err := q.db.Query(ctx, `select `+orgCols+` from orgs o join org_sso s on s.org_id = o.id
		where $1 = any(s.allowed_domains) and o.deleted_at is null order by o.handle`, strings.ToLower(domain))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Identity is a person's account at an identity provider.
type Identity struct {
	Issuer, Subject string
	PersonID        uuid.UUID
	Email           string
	CreatedAt       time.Time
	LastUsedAt      *time.Time
}

// PersonByIdentity finds the person linked to (issuer, subject).
func (q Q) PersonByIdentity(ctx context.Context, issuer, subject string) (Principal, error) {
	p, err := scanPrincipal(q.db.QueryRow(ctx, `select `+prefixed("p", principalCols)+` from principals p
		join person_identities i on i.person_id = p.id where i.issuer = $1 and i.subject = $2`, issuer, subject))
	return p, notFound(err, "person with this identity")
}

// LinkIdentity links (issuer, subject) to a person. ErrExists: the identity
// belongs to someone, or the person has another identity at this issuer.
func (q Q) LinkIdentity(ctx context.Context, person uuid.UUID, issuer, subject, email string) error {
	_, err := q.db.Exec(ctx, `insert into person_identities (issuer, subject, person_id, email, last_used_at) values ($1, $2, $3, $4, now())`,
		issuer, subject, person, email)
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

// TouchIdentity records a sign-in with an identity and its current email.
func (q Q) TouchIdentity(ctx context.Context, issuer, subject, email string) error {
	_, err := q.db.Exec(ctx, `update person_identities set last_used_at = now(), email = $3 where issuer = $1 and subject = $2`, issuer, subject, email)
	return err
}

// IdentitiesOf lists a person's identities, by issuer.
func (q Q) IdentitiesOf(ctx context.Context, person uuid.UUID) ([]Identity, error) {
	rows, err := q.db.Query(ctx, `select issuer, subject, person_id, email, created_at, last_used_at from person_identities where person_id = $1 order by issuer`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Issuer, &i.Subject, &i.PersonID, &i.Email, &i.CreatedAt, &i.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// PeopleByEmail lists the people (enabled ones) whose email is email,
// ignoring case.
func (q Q) PeopleByEmail(ctx context.Context, email string) ([]Principal, error) {
	rows, err := q.db.Query(ctx, `select `+principalCols+` from principals
		where kind = 'human' and disabled_at is null and email <> '' and lower(email) = lower($1) order by created_at`, strings.TrimSpace(email))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkTokenSSO records that token was issued through org's sign-on.
func (q Q) MarkTokenSSO(ctx context.Context, token, org uuid.UUID) error {
	_, err := q.db.Exec(ctx, `insert into token_sso (token_id, org_id) values ($1, $2)
		on conflict (token_id, org_id) do update set verified_at = now()`, token, org)
	return err
}

// CopyTokenSSO gives token to the sign-on marks of token from.
func (q Q) CopyTokenSSO(ctx context.Context, from, to uuid.UUID) error {
	if from == uuid.Nil || to == uuid.Nil {
		return nil
	}
	_, err := q.db.Exec(ctx, `insert into token_sso (token_id, org_id, verified_at)
		select $2, org_id, verified_at from token_sso where token_id = $1
		on conflict (token_id, org_id) do nothing`, from, to)
	return err
}

// SSOBlocked reports whether org requires single sign-on and token was not
// issued through it.
func (q Q) SSOBlocked(ctx context.Context, org, token uuid.UUID) (bool, error) {
	var blocked bool
	err := q.db.QueryRow(ctx, `select exists (select 1 from org_sso s where s.org_id = $1 and s.require_sso
		and not exists (select 1 from token_sso t where t.token_id = $2 and t.org_id = $1))`, org, token).Scan(&blocked)
	return blocked, err
}

// TokenSSOOrgs lists the organizations a token was issued through.
func (q Q) TokenSSOOrgs(ctx context.Context, token uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `select org_id from token_sso where token_id = $1`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UseNonce records an ID token's nonce; false when it was used before.
// Expired nonces are dropped as it goes.
func (q Q) UseNonce(ctx context.Context, key string, expires time.Time) (bool, error) {
	if _, err := q.db.Exec(ctx, `delete from oidc_nonces where expires_at < now()`); err != nil {
		return false, err
	}
	h := sha256.Sum256([]byte(key))
	tag, err := q.db.Exec(ctx, `insert into oidc_nonces (nonce_hash, expires_at) values ($1, $2) on conflict do nothing`, h[:], expires)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetCLILoginApprover records which token approved a CLI login.
func (q Q) SetCLILoginApprover(ctx context.Context, deviceHash []byte, token uuid.UUID) error {
	if token == uuid.Nil {
		return nil
	}
	_, err := q.db.Exec(ctx, `update cli_logins set approved_token_id = $2 where device_hash = $1`, deviceHash, token)
	return err
}

// CLILoginApprover is the token that approved a CLI login (uuid.Nil when
// unknown).
func (q Q) CLILoginApprover(ctx context.Context, deviceHash []byte) (uuid.UUID, error) {
	var id *uuid.UUID
	err := q.db.QueryRow(ctx, `select approved_token_id from cli_logins where device_hash = $1`, deviceHash).Scan(&id)
	if err != nil || id == nil {
		return uuid.Nil, notFound(err, "CLI login")
	}
	return *id, nil
}

// ---------- disabled (merged) people ----------

// PrincipalDisabled reports whether a principal was disabled (merged into
// another) and into whom.
func (q Q) PrincipalDisabled(ctx context.Context, id uuid.UUID) (disabled bool, into *uuid.UUID, err error) {
	var at *time.Time
	err = q.db.QueryRow(ctx, `select disabled_at, merged_into from principals where id = $1`, id).Scan(&at, &into)
	return at != nil, into, notFound(err, "principal")
}

// MergedPeople maps each merged person to the person they were merged into.
func (q Q) MergedPeople(ctx context.Context) (map[uuid.UUID]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `select id, merged_into from principals where merged_into is not null`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]uuid.UUID{}
	for rows.Next() {
		var id, into uuid.UUID
		if err := rows.Scan(&id, &into); err != nil {
			return nil, err
		}
		out[id] = into
	}
	return out, rows.Err()
}

// MergeStats is what MergePeople moved.
type MergeStats struct {
	Tokens, Agents, Orgs, Teams, Grants, Identities int
	GitHub                                          bool
}

// MergePeople moves everything that lets merged act to keep: tokens,
// agents (keeping their handles, so authorship stays readable), organization
// and team memberships (the higher role wins), repository grants (the
// higher role wins), identities and, when keep has none, the GitHub link.
// merged stays as a disabled principal so changes, notes and the op log
// still name them. Run it in a transaction.
func (q Q) MergePeople(ctx context.Context, keep, merged uuid.UUID) (MergeStats, error) {
	var st MergeStats
	// Every statement takes keep as $1 and merged as $2 (the deletes name
	// $1 only so its type is known).
	exec := func(n *int, sql string, args ...any) error {
		tag, err := q.db.Exec(ctx, sql, args...)
		if err == nil && n != nil {
			*n += int(tag.RowsAffected())
		}
		return err
	}
	steps := []struct {
		n   *int
		sql string
	}{
		{&st.Tokens, `update tokens set principal_id = $1 where principal_id = $2`},
		{&st.Agents, `update principals set parent_id = $1 where parent_id = $2`},
		// Memberships: keep's role becomes the higher of the two.
		{&st.Orgs, `insert into org_members (org_id, person_id, role)
			select org_id, $1, role from org_members where person_id = $2
			on conflict (org_id, person_id) do update set role = case
				when org_members.role = 'owner' or excluded.role = 'owner' then 'owner'
				when org_members.role = 'admin' or excluded.role = 'admin' then 'admin'
				else 'member' end`},
		{&st.Teams, `insert into team_members (team_id, org_id, person_id)
			select team_id, org_id, $1 from team_members where person_id = $2
			on conflict do nothing`},
		{&st.Grants, `insert into repo_grants (repo_id, person_id, role)
			select repo_id, $1, role from repo_grants where person_id = $2
			on conflict (repo_id, person_id) where person_id is not null do update set role = case
				when repo_grants.role = 'admin' or excluded.role = 'admin' then 'admin'
				when repo_grants.role = 'write' or excluded.role = 'write' then 'write'
				else 'read' end`},
		{nil, `delete from team_members where person_id = $2 and $1::uuid is not null`},
		{nil, `delete from org_members where person_id = $2 and $1::uuid is not null`},
		{nil, `delete from repo_grants where person_id = $2 and $1::uuid is not null`},
		{&st.Identities, `update person_identities set person_id = $1 where person_id = $2
			and issuer not in (select issuer from person_identities where person_id = $1)`},
		{nil, `delete from person_identities where person_id = $2 and $1::uuid is not null`},
		// Cached GitHub roles of both are stale now.
		{nil, `delete from github_role_cache where principal_id in ($1, $2)`},
		{nil, `delete from github_org_membership_cache where principal_id in ($1, $2)`},
	}
	for _, s := range steps {
		if err := exec(s.n, s.sql, keep, merged); err != nil {
			return st, err
		}
	}
	// GitHub: moves when keep has none; otherwise merged's link is dropped.
	var keepGH, mergedGH *int64
	var mergedLogin *string
	if err := q.db.QueryRow(ctx, `select (select github_user_id from principals where id = $1), github_user_id, github_login from principals where id = $2`,
		keep, merged).Scan(&keepGH, &mergedGH, &mergedLogin); err != nil {
		return st, err
	}
	if _, err := q.db.Exec(ctx, `update principals set github_user_id = null, github_login = null,
		disabled_at = now(), merged_into = $1, site_admin = false where id = $2`, keep, merged); err != nil {
		return st, err
	}
	if keepGH == nil && mergedGH != nil {
		if _, err := q.db.Exec(ctx, `update principals set github_user_id = $2, github_login = $3 where id = $1`, keep, *mergedGH, mergedLogin); err != nil {
			return st, err
		}
		st.GitHub = true
	}
	return st, nil
}

// OrgByID looks an organization up by id; deleted ones are not found.
func (q Q) OrgByID(ctx context.Context, id uuid.UUID) (Org, error) {
	o, err := scanOrg(q.db.QueryRow(ctx, `select `+orgCols+` from orgs o where o.id = $1 and o.deleted_at is null`, id))
	return o, notFound(err, "organization")
}

// TokenOwner is the principal a token belongs to and whether it is still
// active.
func (q Q) TokenOwner(ctx context.Context, token uuid.UUID) (uuid.UUID, bool, error) {
	var owner uuid.UUID
	var revoked *time.Time
	err := q.db.QueryRow(ctx, `select principal_id, revoked_at from tokens where id = $1`, token).Scan(&owner, &revoked)
	return owner, err == nil && revoked == nil, notFound(err, "token")
}

// AnyOrgSSO reports whether any organization has single sign-on.
func (q Q) AnyOrgSSO(ctx context.Context) (bool, error) {
	var ok bool
	err := q.db.QueryRow(ctx, `select exists (select 1 from org_sso)`).Scan(&ok)
	return ok, err
}

// SetLoginCodeToken records which token asked for a vx ui sign-in code.
func (q Q) SetLoginCodeToken(ctx context.Context, codeHash []byte, token uuid.UUID) error {
	if token == uuid.Nil {
		return nil
	}
	_, err := q.db.Exec(ctx, `update login_codes set token_id = $2 where code_hash = $1`, codeHash, token)
	return err
}

// LoginCodeToken is the token that asked for a sign-in code (uuid.Nil when
// none is known or the code does not exist).
func (q Q) LoginCodeToken(ctx context.Context, codeHash []byte) (uuid.UUID, error) {
	var id *uuid.UUID
	err := q.db.QueryRow(ctx, `select token_id from login_codes where code_hash = $1`, codeHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return uuid.Nil, nil
	}
	return *id, err
}
