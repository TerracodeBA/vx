package oidc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/oidc"
	"github.com/TerracodeBA/vx/internal/oidc/oidctest"
)

func claims(p *oidctest.Provider, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{"iss": p.Issuer, "sub": "sub-1", "aud": p.ClientID, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "email": "alice@acme.test", "email_verified": true}
}

// TestVerifyIDToken: good tokens verify; bad signatures, unknown keys,
// other issuers and audiences, expired and future tokens, wrong nonces and
// unsigned tokens are refused; a rotated key is picked up without a
// restart.
func TestVerifyIDToken(t *testing.T) {
	ctx := context.Background()
	p := oidctest.New(t)
	c := &oidc.Client{}
	m, err := c.Discover(ctx, p.Issuer+"/")
	if err != nil {
		t.Fatal(err)
	}
	want := oidc.Expected{Issuer: p.Issuer, ClientID: p.ClientID, Nonce: "n-1"}
	cl, err := c.Verify(ctx, m, p.Sign(claims(p, "n-1")), want)
	if err != nil || cl.Subject != "sub-1" || cl.Email != "alice@acme.test" || cl.EmailVerified == nil || !*cl.EmailVerified {
		t.Fatalf("good token: %+v %v", cl, err)
	}

	bad := func(name string, change func(map[string]any), wantErr error) {
		t.Helper()
		cl := claims(p, "n-1")
		change(cl)
		if _, err := c.Verify(ctx, m, p.Sign(cl), want); !errors.Is(err, wantErr) {
			t.Errorf("%s: err = %v, want %v", name, err, wantErr)
		}
	}
	bad("other issuer", func(c map[string]any) { c["iss"] = "https://evil.example" }, oidc.ErrIssuer)
	bad("other audience", func(c map[string]any) { c["aud"] = "someone-else" }, oidc.ErrAudience)
	bad("several audiences without azp", func(c map[string]any) { c["aud"] = []string{p.ClientID, "other"} }, oidc.ErrAudience)
	bad("expired", func(c map[string]any) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }, oidc.ErrExpired)
	bad("issued in the future", func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() }, oidc.ErrNotYetValid)
	bad("not before", func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }, oidc.ErrNotYetValid)
	bad("wrong nonce", func(c map[string]any) { c["nonce"] = "n-2" }, oidc.ErrNonce)
	bad("no nonce", func(c map[string]any) { delete(c, "nonce") }, oidc.ErrNonce)
	bad("no subject", func(c map[string]any) { c["sub"] = "" }, oidc.ErrNoSubject)
	// Several audiences are fine when azp names vx.
	cl2 := claims(p, "n-1")
	cl2["aud"], cl2["azp"] = []string{"other", p.ClientID}, p.ClientID
	if _, err := c.Verify(ctx, m, p.Sign(cl2), want); err != nil {
		t.Errorf("aud list with azp: %v", err)
	}

	// A signature by another key under the published kid.
	p.BadSignature(true)
	if _, err := c.Verify(ctx, m, p.Sign(claims(p, "n-1")), want); !errors.Is(err, oidc.ErrSignature) {
		t.Errorf("bad signature: %v", err)
	}
	p.BadSignature(false)
	// A changed payload under a good signature.
	tok := p.Sign(claims(p, "n-1"))
	parts := strings.Split(tok, ".")
	forged, _ := json.Marshal(map[string]any{"iss": p.Issuer, "sub": "admin", "aud": p.ClientID, "nonce": "n-1", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := c.Verify(ctx, m, parts[0]+"."+base64.RawURLEncoding.EncodeToString(forged)+"."+parts[2], want); !errors.Is(err, oidc.ErrSignature) {
		t.Errorf("forged payload: %v", err)
	}
	// alg none and HMAC are refused outright.
	for _, alg := range []string{"none", "HS256"} {
		head, _ := json.Marshal(map[string]string{"alg": alg, "kid": "x"})
		body, _ := json.Marshal(claims(p, "n-1"))
		raw := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body) + "."
		if _, err := c.Verify(ctx, m, raw, want); !errors.Is(err, oidc.ErrAlgorithm) {
			t.Errorf("alg %s: %v", alg, err)
		}
	}
	if _, err := c.Verify(ctx, m, "not-a-jwt", want); !errors.Is(err, oidc.ErrMalformed) {
		t.Errorf("malformed: %v", err)
	}

	// Key rotation: a new kid makes vx read the JWKS again.
	p.Rotate(false)
	if _, err := c.Verify(ctx, m, p.Sign(claims(p, "n-1")), want); err != nil {
		t.Errorf("after rotation: %v", err)
	}
	// A token signed with a key the provider no longer publishes.
	old := oidctest.New(t)
	if _, err := c.Verify(ctx, m, old.Sign(claims(p, "n-1")), want); !errors.Is(err, oidc.ErrUnknownKey) {
		t.Errorf("unpublished key: %v", err)
	}
}

// TestVerifyES256: EC keys from a JWKS verify ES256 tokens.
func TestVerifyES256(t *testing.T) {
	ctx := context.Background()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/a", "token_endpoint": issuer + "/t", "jwks_uri": issuer + "/k"})
	})
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
		pt, _ := priv.PublicKey.Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "EC", "crv": "P-256", "kid": "ec1",
			"x": base64.RawURLEncoding.EncodeToString(pt[1:33]), "y": base64.RawURLEncoding.EncodeToString(pt[33:])}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuer = srv.URL
	c := &oidc.Client{}
	m, err := c.Discover(ctx, issuer)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "ec1"})
	body, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "s", "aud": "c", "nonce": "n", "exp": time.Now().Add(time.Minute).Unix()})
	signing := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	if _, err := c.Verify(ctx, m, signing+"."+base64.RawURLEncoding.EncodeToString(sig), oidc.Expected{Issuer: issuer, ClientID: "c", Nonce: "n"}); err != nil {
		t.Fatalf("ES256: %v", err)
	}
}

// TestDiscoverChecksIssuer: a discovery document for another issuer, and
// plain http outside localhost, are refused.
func TestDiscoverChecksIssuer(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://other.example", "authorization_endpoint": "a", "token_endpoint": "t", "jwks_uri": "k"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &oidc.Client{}
	if _, err := c.Discover(ctx, srv.URL); err == nil || !strings.Contains(err.Error(), "names issuer") {
		t.Fatalf("mismatched issuer: %v", err)
	}
	if err := oidc.CheckIssuerURL("http://idp.example.com"); err == nil {
		t.Fatal("http issuer accepted")
	}
	if err := oidc.CheckIssuerURL("https://acme.okta.com"); err != nil {
		t.Fatal(err)
	}
}

// TestExchangeChecksPKCE: the fake provider (like real ones) refuses a
// code without the right verifier or secret.
func TestExchangeChecksPKCE(t *testing.T) {
	ctx := context.Background()
	p := oidctest.New(t)
	c := &oidc.Client{HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	m, err := c.Discover(ctx, p.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	code := func(verifier string) string {
		resp, err := c.HTTP.Get(oidc.AuthorizeURL(m, p.ClientID, "http://vx.test/cb", "st", "n", oidc.Challenge(verifier)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := resp.Location()
		return loc.Query().Get("code")
	}
	if _, err := c.Exchange(ctx, m, p.ClientID, p.ClientSecret, code("right-verifier-0123456789012345678901234567"), "http://vx.test/cb", "wrong"); err == nil || !strings.Contains(err.Error(), "PKCE") {
		t.Fatalf("wrong verifier: %v", err)
	}
	if _, err := c.Exchange(ctx, m, p.ClientID, "wrong-secret", code("v"), "http://vx.test/cb", "v"); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("wrong secret: %v", err)
	}
	raw, err := c.Exchange(ctx, m, p.ClientID, p.ClientSecret, code("v"), "http://vx.test/cb", "v")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(ctx, m, raw, oidc.Expected{Issuer: p.Issuer, ClientID: p.ClientID, Nonce: "n"}); err != nil {
		t.Fatal(err)
	}
}
