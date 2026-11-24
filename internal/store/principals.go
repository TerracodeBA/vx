package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
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

const principalCols = `id, kind, handle, parent_id, display_name, email, created_at`

func scanPrincipal(row interface{ Scan(...any) error }) (Principal, error) {
	var p Principal
	err := row.Scan(&p.ID, &p.Kind, &p.Handle, &p.ParentID, &p.DisplayName, &p.Email, &p.CreatedAt)
	return p, err
}

// CreatePrincipal inserts a principal.
func (q Q) CreatePrincipal(ctx context.Context, p Principal) (Principal, error) {
	if err := ValidateHandle(p.Kind, p.Handle); err != nil {
		return Principal{}, err
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Handle
	}
	row := q.db.QueryRow(ctx, `insert into principals (kind, handle, parent_id, display_name, email)
		values ($1, $2, $3, $4, $5) returning `+principalCols,
		p.Kind, p.Handle, p.ParentID, p.DisplayName, p.Email)
	out, err := scanPrincipal(row)
	if isUniqueViolation(err) {
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
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	secret := TokenPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
	_, err := q.db.Exec(ctx, `insert into tokens (principal_id, secret_hash, name) values ($1, $2, $3)`,
		principalID, hashToken(secret), name)
	if err != nil {
		return "", err
	}
	return secret, nil
}

// Authenticate resolves a token to its principal.
func (q Q) Authenticate(ctx context.Context, secret string) (Principal, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Principal{}, fmt.Errorf("token: %w", ErrNotFound)
	}
	row := q.db.QueryRow(ctx, `update tokens set last_used_at = now()
		where secret_hash = $1 and revoked_at is null
		returning principal_id`, hashToken(secret))
	var id uuid.UUID
	if err := row.Scan(&id); err != nil {
		return Principal{}, notFound(err, "token")
	}
	return q.PrincipalByID(ctx, id)
}
