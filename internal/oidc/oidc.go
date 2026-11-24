// Package oidc is the client side of OpenID Connect sign-in: provider
// discovery, the authorization code flow with PKCE, and ID token
// verification (signature through the provider's JWKS, issuer, audience,
// expiry and nonce). It has no state beyond a key cache and knows nothing
// about vx people.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // SHA-384 and SHA-512 for RS384, ES384 and friends
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Metadata is the part of a provider's discovery document vx uses.
type Metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
}

// Client talks to providers. The zero value works, with a 10 second
// timeout.
type Client struct {
	HTTP *http.Client
	// Now is the clock for expiry checks; nil: time.Now.
	Now func() time.Time

	mu   sync.Mutex
	meta map[string]metaEntry      // by issuer
	keys map[string]map[string]jwk // by JWKS URL, then kid
	seen map[string]time.Time      // when each JWKS URL was fetched
}

type metaEntry struct {
	m  Metadata
	at time.Time
}

// metadataTTL is how long discovery documents are kept.
const metadataTTL = time.Hour

// Skew is how much clock difference expiry checks allow.
const Skew = 2 * time.Minute

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// NormalizeIssuer trims spaces and a trailing slash.
func NormalizeIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/")
}

// CheckIssuerURL says what is wrong with an issuer URL: it must be https,
// except on localhost for development.
func CheckIssuerURL(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return fmt.Errorf("issuer %q is not a URL; it looks like https://acme.okta.com or https://accounts.google.com", issuer)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer %q must not have a query or fragment", issuer)
	}
	if u.Scheme == "https" {
		return nil
	}
	if h := u.Hostname(); u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1") {
		return nil
	}
	return fmt.Errorf("issuer %q must use https", issuer)
}

// Discover reads the provider's discovery document, cached for an hour. The
// document must name the same issuer.
func (c *Client) Discover(ctx context.Context, issuer string) (Metadata, error) {
	issuer = NormalizeIssuer(issuer)
	if err := CheckIssuerURL(issuer); err != nil {
		return Metadata{}, err
	}
	c.mu.Lock()
	if e, ok := c.meta[issuer]; ok && c.now().Sub(e.at) < metadataTTL {
		c.mu.Unlock()
		return e.m, nil
	}
	c.mu.Unlock()
	var m Metadata
	if err := c.getJSON(ctx, issuer+"/.well-known/openid-configuration", &m); err != nil {
		return Metadata{}, fmt.Errorf("reading the discovery document of %s: %w", issuer, err)
	}
	if NormalizeIssuer(m.Issuer) != issuer {
		return Metadata{}, fmt.Errorf("the discovery document of %s names issuer %q; use exactly the issuer the provider names", issuer, m.Issuer)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return Metadata{}, fmt.Errorf("the discovery document of %s lacks an authorization endpoint, token endpoint or jwks_uri", issuer)
	}
	c.mu.Lock()
	if c.meta == nil {
		c.meta = map[string]metaEntry{}
	}
	c.meta[issuer] = metaEntry{m: m, at: c.now()}
	c.mu.Unlock()
	return m, nil
}

func (c *Client) getJSON(ctx context.Context, link string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", link, resp.Status)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s did not answer JSON: %w", link, err)
	}
	return nil
}

// AuthorizeURL is where the browser goes to sign in: the code flow with
// PKCE (S256), the openid, email and profile scopes, state and nonce.
func AuthorizeURL(m Metadata, clientID, redirectURI, state, nonce, challenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return m.AuthorizationEndpoint + sep + q.Encode()
}

// Challenge is the S256 PKCE challenge of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ProviderError is an error answer of the token endpoint.
type ProviderError struct {
	Code, Description string
}

func (e *ProviderError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// Exchange trades an authorization code for the provider's ID token,
// authenticating with the client secret (HTTP basic unless the provider
// only takes it in the form).
func (c *Client) Exchange(ctx context.Context, m Metadata, clientID, clientSecret, code, redirectURI, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	post := len(m.TokenAuthMethods) > 0 && !slices.Contains(m.TokenAuthMethods, "client_secret_basic") && slices.Contains(m.TokenAuthMethods, "client_secret_post")
	if post {
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if !post {
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Error != "" {
		return "", &ProviderError{Code: out.Error, Description: out.ErrorDescription}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the token endpoint answered %s", resp.Status)
	}
	if out.IDToken == "" {
		return "", errors.New("the token endpoint sent no ID token; is the openid scope allowed for this client?")
	}
	return out.IDToken, nil
}

// Claims are the ID token claims vx reads.
type Claims struct {
	Issuer        string
	Subject       string
	Audience      []string
	Expiry        time.Time
	IssuedAt      time.Time
	Nonce         string
	Email         string
	EmailVerified *bool // nil when the provider does not say
	Name          string
}

// Expected is what an ID token must say.
type Expected struct {
	Issuer   string
	ClientID string
	Nonce    string
}

// Verification errors, so callers and tests can tell them apart.
var (
	ErrMalformed    = errors.New("the ID token is malformed")
	ErrAlgorithm    = errors.New("the ID token uses a signing algorithm vx does not accept")
	ErrUnknownKey   = errors.New("the ID token is signed with a key the provider does not publish")
	ErrSignature    = errors.New("the ID token's signature does not verify")
	ErrIssuer       = errors.New("the ID token comes from another issuer")
	ErrAudience     = errors.New("the ID token is for another client")
	ErrExpired      = errors.New("the ID token has expired")
	ErrNotYetValid  = errors.New("the ID token is not valid yet")
	ErrNonce        = errors.New("the ID token's nonce does not match this sign-in")
	ErrNoSubject    = errors.New("the ID token names no subject")
	errNoJWKSAnswer = errors.New("could not read the provider's signing keys")
)

// Verify checks an ID token: its signature against the provider's JWKS
// (fetched again when it names a key vx has not seen, so key rotation
// works), issuer, audience (and azp when there are several), expiry,
// issued-at and nonce.
func (c *Client) Verify(ctx context.Context, m Metadata, raw string, want Expected) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Claims{}, ErrMalformed
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &head) != nil {
		return Claims{}, ErrMalformed
	}
	hash, ok := algHash[head.Alg]
	if !ok {
		return Claims{}, fmt.Errorf("%w (%q)", ErrAlgorithm, head.Alg)
	}
	key, err := c.key(ctx, m.JWKSURI, head.Kid, head.Alg)
	if err != nil {
		return Claims{}, err
	}
	h := hash.New()
	h.Write([]byte(parts[0] + "." + parts[1]))
	if !verifySig(key, head.Alg, hash, h.Sum(nil), sig) {
		return Claims{}, ErrSignature
	}
	var cl struct {
		Iss           string          `json:"iss"`
		Sub           string          `json:"sub"`
		Aud           json.RawMessage `json:"aud"`
		Azp           string          `json:"azp"`
		Exp           json.Number     `json:"exp"`
		Iat           json.Number     `json:"iat"`
		Nbf           json.Number     `json:"nbf"`
		Nonce         string          `json:"nonce"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
		Name          string          `json:"name"`
	}
	dec := json.NewDecoder(strings.NewReader(string(pb)))
	dec.UseNumber()
	if dec.Decode(&cl) != nil {
		return Claims{}, ErrMalformed
	}
	out := Claims{Issuer: cl.Iss, Subject: cl.Sub, Nonce: cl.Nonce, Email: strings.TrimSpace(cl.Email), Name: strings.TrimSpace(cl.Name)}
	var one string
	if json.Unmarshal(cl.Aud, &one) == nil {
		out.Audience = []string{one}
	} else if json.Unmarshal(cl.Aud, &out.Audience) != nil {
		return Claims{}, ErrMalformed
	}
	switch v := strings.Trim(string(cl.EmailVerified), `"`); v {
	case "true":
		t := true
		out.EmailVerified = &t
	case "false":
		f := false
		out.EmailVerified = &f
	}
	if NormalizeIssuer(cl.Iss) != NormalizeIssuer(want.Issuer) {
		return Claims{}, ErrIssuer
	}
	if !slices.Contains(out.Audience, want.ClientID) || (len(out.Audience) > 1 && cl.Azp != want.ClientID) {
		return Claims{}, ErrAudience
	}
	now := c.now()
	exp, ok := unix(cl.Exp)
	if !ok {
		return Claims{}, ErrMalformed
	}
	out.Expiry = exp
	if !now.Before(exp.Add(Skew)) {
		return Claims{}, ErrExpired
	}
	if iat, ok := unix(cl.Iat); ok {
		out.IssuedAt = iat
		if iat.After(now.Add(Skew)) {
			return Claims{}, ErrNotYetValid
		}
	}
	if nbf, ok := unix(cl.Nbf); ok && nbf.After(now.Add(Skew)) {
		return Claims{}, ErrNotYetValid
	}
	if want.Nonce == "" || subtle.ConstantTimeCompare([]byte(cl.Nonce), []byte(want.Nonce)) != 1 {
		return Claims{}, ErrNonce
	}
	if out.Subject == "" {
		return Claims{}, ErrNoSubject
	}
	return out, nil
}

func unix(n json.Number) (time.Time, bool) {
	if n == "" {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

var algHash = map[string]crypto.Hash{
	"RS256": crypto.SHA256, "RS384": crypto.SHA384, "RS512": crypto.SHA512,
	"PS256": crypto.SHA256, "PS384": crypto.SHA384, "PS512": crypto.SHA512,
	"ES256": crypto.SHA256, "ES384": crypto.SHA384,
}

func verifySig(key crypto.PublicKey, alg string, hash crypto.Hash, digest, sig []byte) bool {
	switch k := key.(type) {
	case *rsa.PublicKey:
		switch alg[:2] {
		case "RS":
			return rsa.VerifyPKCS1v15(k, hash, digest, sig) == nil
		case "PS":
			return rsa.VerifyPSS(k, hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}
	case *ecdsa.PublicKey:
		if alg[:2] != "ES" {
			return false
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return false
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		return ecdsa.Verify(k, digest, r, s)
	}
	return false
}

// jwk is one parsed key of a JWKS.
type jwk struct {
	key crypto.PublicKey
	alg string // the key's alg, if it names one
}

// key returns the key kid of the JWKS at link, fetching the set again when
// kid is unknown (the provider rotated its keys).
func (c *Client) key(ctx context.Context, link, kid, alg string) (crypto.PublicKey, error) {
	find := func() (crypto.PublicKey, bool, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		set, ok := c.keys[link]
		if !ok {
			return nil, false, false
		}
		if kid == "" && len(set) == 1 {
			for _, k := range set {
				return k.key, true, true
			}
		}
		k, ok := set[kid]
		if !ok {
			return nil, false, true
		}
		if k.alg != "" && k.alg != alg {
			return nil, false, true
		}
		return k.key, true, true
	}
	if k, ok, _ := find(); ok {
		return k, nil
	}
	if err := c.fetchKeys(ctx, link); err != nil {
		return nil, err
	}
	if k, ok, _ := find(); ok {
		return k, nil
	}
	return nil, ErrUnknownKey
}

func (c *Client) fetchKeys(ctx context.Context, link string) error {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := c.getJSON(ctx, link, &set); err != nil {
		return fmt.Errorf("%w: %v", errNoJWKSAnswer, err)
	}
	keys := map[string]jwk{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		var pub crypto.PublicKey
		switch k.Kty {
		case "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(e) > 4 {
				continue
			}
			pk := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
			if pk.N.BitLen() < 2048 {
				continue // too weak to trust
			}
			pub = pk
		case "EC":
			var curve elliptic.Curve
			switch k.Crv {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			default:
				continue
			}
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			size := (curve.Params().BitSize + 7) / 8
			if err1 != nil || err2 != nil || len(x) > size || len(y) > size {
				continue
			}
			point := make([]byte, 1+2*size)
			point[0] = 4
			copy(point[1+size-len(x):1+size], x)
			copy(point[1+2*size-len(y):], y)
			pk, err := ecdsa.ParseUncompressedPublicKey(curve, point)
			if err != nil {
				continue // not on the curve
			}
			pub = pk
		default:
			continue
		}
		keys[k.Kid] = jwk{key: pub, alg: k.Alg}
	}
	c.mu.Lock()
	if c.keys == nil {
		c.keys, c.seen = map[string]map[string]jwk{}, map[string]time.Time{}
	}
	c.keys[link] = keys
	c.seen[link] = c.now()
	c.mu.Unlock()
	return nil
}
