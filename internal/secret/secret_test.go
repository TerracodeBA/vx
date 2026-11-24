package secret_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/secret"
)

func TestSecretSealOpen(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	a, err := secret.SealString(key, "github_app.private_key", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := secret.SealString(key, "github_app.private_key", "hunter2")
	if bytes.Equal(a, b) || bytes.Contains(a, []byte("hunter2")) {
		t.Fatal("sealing is not randomized or leaks the plaintext")
	}
	if got, err := secret.OpenString(key, "github_app.private_key", a); err != nil || got != "hunter2" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := secret.Open(key, "github_app.client_secret", a); !errors.Is(err, secret.ErrOpen) {
		t.Fatalf("other purpose opened: %v", err)
	}
	other := make([]byte, 32)
	rand.Read(other)
	if _, err := secret.Open(other, "github_app.private_key", a); !errors.Is(err, secret.ErrOpen) {
		t.Fatalf("other key opened: %v", err)
	}
	a[len(a)-1] ^= 1
	if _, err := secret.Open(key, "github_app.private_key", a); !errors.Is(err, secret.ErrOpen) {
		t.Fatalf("tampered value opened: %v", err)
	}
	if _, err := secret.Seal(nil, "x", []byte("y")); !errors.Is(err, secret.ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := secret.Open(key, "x", []byte{1, 2}); !errors.Is(err, secret.ErrOpen) {
		t.Fatalf("short value: %v", err)
	}
}

func TestSecretParseKey(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	for _, s := range []string{
		hex.EncodeToString(key), strings.ToUpper(hex.EncodeToString(key)) + "\n",
		base64.StdEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key),
	} {
		got, err := secret.ParseKey(s)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("ParseKey(%q) = %x, %v", s, got, err)
		}
	}
	if _, err := secret.ParseKey(""); !errors.Is(err, secret.ErrNoKey) {
		t.Errorf("empty: %v", err)
	}
	for _, s := range []string{"abc", hex.EncodeToString(key[:16]), base64.StdEncoding.EncodeToString(key[:31])} {
		_, err := secret.ParseKey(s)
		if err == nil || strings.Contains(err.Error(), s) {
			t.Errorf("ParseKey(%q): %v", s, err)
		}
	}
}
