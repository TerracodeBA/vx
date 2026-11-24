// Package secret seals secrets at rest with the server key (VX_SECRET_KEY):
// AES-256-GCM with a random nonce. Every database column ending in _enc
// holds a sealed value, never plaintext.
//
// A sealed value is: version byte (1), 12-byte nonce, ciphertext and tag.
// The caller passes associated data naming what the value is (for example
// "github_app.private_key"), so a value copied into another column does not
// open.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the size of the server key in bytes.
const KeySize = 32

const version = 1

// ErrNoKey is returned when sealing or opening without a server key.
var ErrNoKey = errors.New("VX_SECRET_KEY is not set: the server needs a 32-byte key (64 hex characters or base64) to store secrets; generate one with `openssl rand -hex 32`")

// ErrOpen is returned when a sealed value does not open: it was sealed with
// another key, for another purpose, or it was changed.
var ErrOpen = errors.New("a stored secret does not open with VX_SECRET_KEY: the key changed or the value is damaged")

// ParseKey reads a server key: 64 hex characters, or base64 (standard or
// URL alphabet, padded or not) of 32 bytes. Surrounding space is ignored.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrNoKey
	}
	if len(s) == 2*KeySize {
		if k, err := hex.DecodeString(s); err == nil {
			return k, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == KeySize {
			return k, nil
		}
	}
	return nil, fmt.Errorf("VX_SECRET_KEY must be 32 bytes as 64 hex characters or base64 (got %d characters); generate one with `openssl rand -hex 32`", len(s))
}

func aead(key []byte) (cipher.AEAD, error) {
	if len(key) == 0 {
		return nil, ErrNoKey
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("the server key must be %d bytes, not %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with key; purpose is bound as associated data.
func Seal(key []byte, purpose string, plaintext []byte) ([]byte, error) {
	g, err := aead(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+g.NonceSize(), 1+g.NonceSize()+len(plaintext)+g.Overhead())
	out[0] = version
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	return g.Seal(out, out[1:1+g.NonceSize()], plaintext, []byte(purpose)), nil
}

// Open decrypts a value from Seal with the same key and purpose.
func Open(key []byte, purpose string, sealed []byte) ([]byte, error) {
	g, err := aead(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 1+g.NonceSize()+g.Overhead() || sealed[0] != version {
		return nil, ErrOpen
	}
	nonce := sealed[1 : 1+g.NonceSize()]
	pt, err := g.Open(nil, nonce, sealed[1+g.NonceSize():], []byte(purpose))
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// SealString and OpenString are Seal and Open for strings.
func SealString(key []byte, purpose, plaintext string) ([]byte, error) {
	return Seal(key, purpose, []byte(plaintext))
}

func OpenString(key []byte, purpose string, sealed []byte) (string, error) {
	b, err := Open(key, purpose, sealed)
	return string(b), err
}
