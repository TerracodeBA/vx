package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PrincipalKind is the type of an identity.
type PrincipalKind string

const (
	KindHuman   PrincipalKind = "human"
	KindAgent   PrincipalKind = "agent"
	KindService PrincipalKind = "service"
)

// Principal is a human, an agent acting for a human, or a service account.
type Principal struct {
	ID          uuid.UUID
	Kind        PrincipalKind
	Handle      string // "alice" or "alice/claude-backend"
	ParentID    *uuid.UUID
	DisplayName string
	Email       string
	SiteAdmin   bool // may manage people and every organization (humans only)
	CreatedAt   time.Time
}

// Ref is the principal's reference string, as recorded on changesets:
// "user:alice", "agent:alice/claude-backend" or "service:ci".
func (p Principal) Ref() string {
	prefix := "user"
	switch p.Kind {
	case KindAgent:
		prefix = "agent"
	case KindService:
		prefix = "service"
	}
	return prefix + ":" + p.Handle
}

var handlePart = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,38}$`)

// ValidateHandle checks a handle: lowercase name, and for agents
// "owner/name".
func ValidateHandle(kind PrincipalKind, handle string) error {
	parts := strings.Split(handle, "/")
	want := 1
	if kind == KindAgent {
		want = 2
	}
	if len(parts) != want {
		if kind == KindAgent {
			return fmt.Errorf("agent handle %q must look like owner/name", handle)
		}
		return fmt.Errorf("handle %q must not contain /", handle)
	}
	for _, p := range parts {
		if !handlePart.MatchString(p) {
			return fmt.Errorf("handle %q: use lowercase letters, digits, dot, dash or underscore", handle)
		}
	}
	return nil
}

const principalCols = `id, kind, handle, parent_id, display_name, email, site_admin, created_at`

func scanPrincipal(row interface{ Scan(...any) error }) (Principal, error) {
	var p Principal
	err := row.Scan(&p.ID, &p.Kind, &p.Handle, &p.ParentID, &p.DisplayName, &p.Email, &p.SiteAdmin, &p.CreatedAt)
	return p, err
}

// CreatePrincipal inserts a principal. A person (KindHuman) also gets their
// personal organization, named like them and owned by them, in the same
// statement; SiteAdmin is only kept for people.
func (q Q) CreatePrincipal(ctx context.Context, p Principal) (Principal, error) {
	if err := ValidateHandle(p.Kind, p.Handle); err != nil {
		return Principal{}, err
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Handle
	}
	row := q.db.QueryRow(ctx, `with p as (
			insert into principals (kind, handle, parent_id, display_name, email, site_admin)
			values ($1, $2, $3, $4, $5, $6 and $1 = 'human') returning `+principalCols+`
		), o as (
			insert into orgs (handle, display_name, default_repo_role, personal)
			select handle, display_name, 'none', true from p where kind = 'human'
			returning id
		), m as (
			insert into org_members (org_id, person_id, role) select o.id, p.id, 'owner' from o, p
		)
		select `+principalCols+` from p`,
		p.Kind, p.Handle, p.ParentID, p.DisplayName, p.Email, p.SiteAdmin)
	out, err := scanPrincipal(row)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		if pg.ConstraintName == "orgs_handle_key" {
			return Principal{}, fmt.Errorf("handle %q is taken by an organization: %w", p.Handle, ErrExists)
		}
		return Principal{}, fmt.Errorf("principal %q: %w", p.Handle, ErrExists)
	}
	return out, err
}

// PrincipalByHandle looks a principal up by handle.
func (q Q) PrincipalByHandle(ctx context.Context, handle string) (Principal, error) {
	p, err := scanPrincipal(q.db.QueryRow(ctx, `select `+principalCols+` from principals where handle = $1`, handle))
	return p, notFound(err, "principal "+handle)
}

// PrincipalByID looks a principal up by id.
func (q Q) PrincipalByID(ctx context.Context, id uuid.UUID) (Principal, error) {
	p, err := scanPrincipal(q.db.QueryRow(ctx, `select `+principalCols+` from principals where id = $1`, id))
	return p, notFound(err, "principal")
}

// ListAgents returns the agents acting for owner.
func (q Q) ListAgents(ctx context.Context, owner uuid.UUID) ([]Principal, error) {
	rows, err := q.db.Query(ctx, `select `+principalCols+` from principals where parent_id = $1 order by handle`, owner)
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

// TokenPrefix starts every vx token, so leaked tokens are easy to spot.
const TokenPrefix = "vxt_"

func hashToken(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreateToken issues a new token for a principal and returns the secret.
// Only its hash is stored, so the secret cannot be shown again.
func (q Q) CreateToken(ctx context.Context, principalID uuid.UUID, name string) (string, error) {
	_, secret, err := q.IssueToken(ctx, principalID, name)
	return secret, err
}

// IssueToken is CreateToken that also returns the new token's record.
func (q Q) IssueToken(ctx context.Context, principalID uuid.UUID, name string) (Token, string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Token{}, "", err
	}
	secret := TokenPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
	tok, err := scanToken(q.db.QueryRow(ctx, `insert into tokens (principal_id, secret_hash, name) values ($1, $2, $3)
		returning `+tokenCols, principalID, hashToken(secret), name))
	if err != nil {
		return Token{}, "", err
	}
	return tok, secret, nil
}

// Authenticate resolves a token to its principal.
func (q Q) Authenticate(ctx context.Context, secret string) (Principal, error) {
	p, _, err := q.AuthenticateToken(ctx, secret)
	return p, err
}

// AuthenticateToken resolves a token to its principal and also returns the
// token's id, so callers know which token a request was made with.
func (q Q) AuthenticateToken(ctx context.Context, secret string) (Principal, uuid.UUID, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Principal{}, uuid.Nil, fmt.Errorf("token: %w", ErrNotFound)
	}
	row := q.db.QueryRow(ctx, `update tokens set last_used_at = now()
		where secret_hash = $1 and revoked_at is null
			and not exists (select 1 from principals d where d.id = tokens.principal_id and d.disabled_at is not null)
		returning id, principal_id`, hashToken(secret))
	var tokenID, id uuid.UUID
	if err := row.Scan(&tokenID, &id); err != nil {
		return Principal{}, uuid.Nil, notFound(err, "token")
	}
	p, err := q.PrincipalByID(ctx, id)
	return p, tokenID, err
}

// Token describes a token without its secret, which is never stored.
type Token struct {
	ID          uuid.UUID
	PrincipalID uuid.UUID
	Name        string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

// Active reports whether the token can still be used.
func (t Token) Active() bool { return t.RevokedAt == nil }

const tokenCols = `id, principal_id, name, created_at, last_used_at, revoked_at`

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	err := row.Scan(&t.ID, &t.PrincipalID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	return t, err
}

// ListTokens returns the tokens of the given principals, active ones first,
// then newest first.
func (q Q) ListTokens(ctx context.Context, principalIDs ...uuid.UUID) ([]Token, error) {
	rows, err := q.db.Query(ctx, `select `+tokenCols+` from tokens where principal_id = any($1)
		order by revoked_at is not null, created_at desc, id`, principalIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenByID looks a token up by id.
func (q Q) TokenByID(ctx context.Context, id uuid.UUID) (Token, error) {
	t, err := scanToken(q.db.QueryRow(ctx, `select `+tokenCols+` from tokens where id = $1`, id))
	return t, notFound(err, "token "+id.String())
}

// RevokeToken marks a token revoked and returns it. Revoking a token twice
// keeps the first revocation time; revoked reports whether this call did it.
func (q Q) RevokeToken(ctx context.Context, id uuid.UUID) (t Token, revoked bool, err error) {
	t, err = scanToken(q.db.QueryRow(ctx, `update tokens set revoked_at = now()
		where id = $1 and revoked_at is null returning `+tokenCols, id))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Token{}, false, err
	}
	t, err = q.TokenByID(ctx, id)
	return t, false, err
}
