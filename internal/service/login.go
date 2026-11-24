package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/store"
)

// LoginCodeTTL is how long a one-time web sign-in code stays valid.
const LoginCodeTTL = 2 * time.Minute

// One-time codes that sign a principal in to the web UI (vx ui) live in
// Postgres (login_codes), so a code issued by one server works on another
// behind the same load balancer. Only a hash of each code is kept.

func hashCode(code string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return sum[:]
}

// CreateLoginCode returns a one-time code that signs the actor in to the web
// UI. It expires after LoginCodeTTL and works once.
func (s *Service) CreateLoginCode(ctx context.Context, actor Actor) (string, time.Time, error) {
	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	code := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
	now := s.Now()
	expires := now.Add(LoginCodeTTL)
	if err := s.Store.CreateLoginCode(ctx, hashCode(code), actor.ID, expires, now); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

func errLoginCode() error {
	return errf(CodeNotFound, "this sign-in link has expired or was already used; run vx ui again")
}

// PeekLoginCode tells who a code would sign in, without using it up.
func (s *Service) PeekLoginCode(ctx context.Context, code string) (store.Principal, error) {
	id, err := s.Store.PeekLoginCode(ctx, hashCode(code), s.Now())
	if errors.Is(err, store.ErrNotFound) {
		return store.Principal{}, errLoginCode()
	}
	if err != nil {
		return store.Principal{}, err
	}
	return s.Store.PrincipalByID(ctx, id)
}

// RedeemLoginCode uses up a code and issues a new token for its principal,
// to be kept in the browser's session cookie.
func (s *Service) RedeemLoginCode(ctx context.Context, code string) (store.Principal, string, error) {
	id, err := s.Store.UseLoginCode(ctx, hashCode(code), s.Now())
	if errors.Is(err, store.ErrNotFound) {
		return store.Principal{}, "", errLoginCode()
	}
	if err != nil {
		return store.Principal{}, "", err
	}
	p, err := s.Store.PrincipalByID(ctx, id)
	if err != nil {
		return store.Principal{}, "", classify(err)
	}
	var token string
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if token, err = tx.CreateToken(ctx, p.ID, "web session (vx ui)"); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &p.ID, "web.login", p.Ref(), nil, map[string]string{"via": "vx ui"})
	})
	return p, token, classify(err)
}
