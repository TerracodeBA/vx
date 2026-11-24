package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// LoginCodeTTL is how long a one-time web sign-in code stays valid.
const LoginCodeTTL = 2 * time.Minute

// loginCodes holds one-time codes that sign a principal in to the web UI
// (vx ui). They live in memory: a code is meant to be used seconds after it
// is created, by the same server. Only a hash of each code is kept.
type loginCodes struct {
	mu    sync.Mutex
	codes map[[32]byte]loginCode
}

type loginCode struct {
	principal uuid.UUID
	expires   time.Time
}

func hashCode(code string) [32]byte { return sha256.Sum256([]byte(strings.TrimSpace(code))) }

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
	s.logins.mu.Lock()
	if s.logins.codes == nil {
		s.logins.codes = map[[32]byte]loginCode{}
	}
	for k, c := range s.logins.codes {
		if now.After(c.expires) {
			delete(s.logins.codes, k)
		}
	}
	s.logins.codes[hashCode(code)] = loginCode{principal: actor.ID, expires: expires}
	s.logins.mu.Unlock()
	return code, expires, nil
}

// PeekLoginCode tells who a code would sign in, without using it up.
func (s *Service) PeekLoginCode(ctx context.Context, code string) (store.Principal, error) {
	s.logins.mu.Lock()
	c, ok := s.logins.codes[hashCode(code)]
	s.logins.mu.Unlock()
	if !ok || s.Now().After(c.expires) {
		return store.Principal{}, errf(CodeNotFound, "this sign-in link has expired or was already used; run vx ui again")
	}
	return s.Store.PrincipalByID(ctx, c.principal)
}

// RedeemLoginCode uses up a code and issues a new token for its principal,
// to be kept in the browser's session cookie.
func (s *Service) RedeemLoginCode(ctx context.Context, code string) (store.Principal, string, error) {
	h := hashCode(code)
	s.logins.mu.Lock()
	c, ok := s.logins.codes[h]
	delete(s.logins.codes, h)
	s.logins.mu.Unlock()
	if !ok || s.Now().After(c.expires) {
		return store.Principal{}, "", errf(CodeNotFound, "this sign-in link has expired or was already used; run vx ui again")
	}
	p, err := s.Store.PrincipalByID(ctx, c.principal)
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
