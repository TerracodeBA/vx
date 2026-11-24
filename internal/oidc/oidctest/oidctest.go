// Package oidctest is a fake OpenID Connect provider for tests: discovery,
// JWKS, an authorize endpoint that signs the configured user in at once,
// and a token endpoint that checks the client secret and PKCE and issues
// RS256 ID tokens. Tests rotate keys and break tokens on purpose.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// User is who the fake provider signs in.
type User struct {
	Subject       string
	Email         string
	EmailVerified *bool // nil: the claim is left out
	Name          string
}

// Verified is a pointer to true, for User.EmailVerified.
func Verified(v bool) *bool { return &v }

// Provider is a running fake provider.
type Provider struct {
	t      testing.TB
	srv    *httptest.Server
	Issuer string

	ClientID, ClientSecret string

	mu    sync.Mutex
	keys  []*key // published; the first one signs
	user  User
	codes map[string]grant
	// Tamper changes the claims of the next ID tokens before signing.
	tamper func(map[string]any)
	// badSig signs with a key that is not published under the kid.
	badSig bool
	// Authorizations counts authorize requests.
	Authorizations int
	// LastChallenge is the PKCE challenge of the last authorize request.
	LastChallenge string
}

type key struct {
	kid  string
	priv *rsa.PrivateKey
}

type grant struct {
	user                         User
	nonce, challenge, redirectTo string
	clientID                     string
}

// New starts a provider with one signing key and the user
// sub-1 / alice@acme.test (verified).
func New(t testing.TB) *Provider {
	p := &Provider{t: t, ClientID: "vx-client", ClientSecret: "s3cret", codes: map[string]grant{},
		user: User{Subject: "sub-1", Email: "alice@acme.test", EmailVerified: Verified(true), Name: "Alice Acme"}}
	p.keys = []*key{p.newKey()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	p.Issuer = p.srv.URL
	return p
}

func (p *Provider) newKey() *key {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		p.t.Fatal(err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &key{kid: base64.RawURLEncoding.EncodeToString(b), priv: priv}
}

// SetUser sets who the next sign-ins are for.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	p.user = u
	p.mu.Unlock()
}

// Rotate publishes a new signing key; the old one stays published when
// keepOld is set (the usual overlap), otherwise it is dropped.
func (p *Provider) Rotate(keepOld bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.newKey()
	if keepOld {
		p.keys = append([]*key{k}, p.keys...)
	} else {
		p.keys = []*key{k}
	}
}

// Tamper changes the claims of later ID tokens (nil stops).
func (p *Provider) Tamper(fn func(claims map[string]any)) {
	p.mu.Lock()
	p.tamper = fn
	p.mu.Unlock()
}

// BadSignature makes later ID tokens carry a signature by an unpublished
// key under the published kid.
func (p *Provider) BadSignature(on bool) {
	p.mu.Lock()
	p.badSig = on
	p.mu.Unlock()
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.Issuer,
		"authorization_endpoint":                p.Issuer + "/authorize",
		"token_endpoint":                        p.Issuer + "/token",
		"jwks_uri":                              p.Issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var keys []map[string]string
	for _, k := range p.keys {
		pub := k.priv.PublicKey
		keys = append(keys, map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": k.kid,
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	writeJSON(w, map[string]any{"keys": keys})
}

// authorize signs the configured user in at once and sends the browser
// back with a code.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		http.Error(w, "need response_type=code, a nonce and an S256 code challenge", http.StatusBadRequest)
		return
	}
	if q.Get("client_id") != p.ClientID {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	code := randomString()
	p.mu.Lock()
	p.Authorizations++
	p.LastChallenge = q.Get("code_challenge")
	p.codes[code] = grant{user: p.user, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectTo: q.Get("redirect_uri"), clientID: q.Get("client_id")}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	p.mu.Lock()
	g, found := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	if !found {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "unknown or used code")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if r.PostForm.Get("redirect_uri") != g.redirectTo {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": p.Issuer, "sub": g.user.Subject, "aud": g.clientID, "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	if g.user.Email != "" {
		claims["email"] = g.user.Email
	}
	if g.user.EmailVerified != nil {
		claims["email_verified"] = *g.user.EmailVerified
	}
	if g.user.Name != "" {
		claims["name"] = g.user.Name
	}
	writeJSON(w, map[string]any{"access_token": randomString(), "token_type": "Bearer", "id_token": p.Sign(claims)})
}

// Sign makes an ID token from claims with the current key (applying Tamper
// and BadSignature), for tests that call a verifier directly.
func (p *Provider) Sign(claims map[string]any) string {
	p.mu.Lock()
	k := p.keys[0]
	tamper, bad := p.tamper, p.badSig
	p.mu.Unlock()
	if tamper != nil {
		tamper(claims)
	}
	priv := k.priv
	if bad {
		priv = p.newKey().priv
	}
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.kid})
	body, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func tokenError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q,"error_description":%q}`, code, desc)
}
