// Package webpush sends Web Push messages: the payload encrypted for one
// browser subscription (RFC 8291, aes128gcm of RFC 8188) and the request
// signed with the server's VAPID key (RFC 8292). It uses the standard
// library only.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Subscription is what a browser's PushManager.subscribe returns: where to
// send, and the keys to encrypt for. P256dh and Auth are base64url.
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// ErrGone means the push service no longer knows the subscription (404 or
// 410): the browser unsubscribed or the person revoked permission. Drop it.
var ErrGone = errors.New("the push subscription expired or was revoked")

// MaxPayload is the largest plaintext a message may carry: 4096-byte
// records minus the header, padding delimiter and tag leave about 3993,
// and push services accept 4096 bytes of body at most.
const MaxPayload = 3000

// GenerateKey makes a new VAPID key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// ParseKey reads a VAPID key from its 32-byte private scalar.
func ParseKey(raw []byte) (*ecdsa.PrivateKey, error) {
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
}

// KeyBytes is the 32-byte private scalar of a VAPID key, for sealing.
func KeyBytes(k *ecdsa.PrivateKey) ([]byte, error) { return k.Bytes() }

// PublicKey is the key's public point, uncompressed and base64url: the
// applicationServerKey a browser subscribes with.
func PublicKey(k *ecdsa.PrivateKey) (string, error) {
	b, err := k.PublicKey.Bytes()
	if err != nil {
		return "", err
	}
	return b64(b), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// decode reads base64url with or without padding (browsers send either).
func decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// CheckSubscription reports why a subscription cannot be used, or nil.
// The endpoint must be https on a public host name: the server posts to it,
// so it must not reach into the server's own network.
func CheckSubscription(sub Subscription) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("the push endpoint must be an https URL")
	}
	if len(sub.Endpoint) > 1024 {
		return errors.New("the push endpoint is longer than 1024 characters")
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.EqualFold(host, "localhost") {
		return fmt.Errorf("the push endpoint's host %q is not a public push service", host)
	}
	pub, err := decode(sub.P256dh)
	if err != nil {
		return errors.New("the subscription's p256dh key is not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("the subscription's p256dh key is not a P-256 point")
	}
	auth, err := decode(sub.Auth)
	if err != nil || len(auth) != 16 {
		return errors.New("the subscription's auth secret must be 16 bytes of base64url")
	}
	return nil
}

// Encrypt encrypts plaintext for a subscription with a fresh key and salt.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	ua, err := decode(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	auth, err := decode(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(ua, auth, salt, as, plaintext)
}

// encrypt is RFC 8291 section 3.4 with one record of 4096 bytes.
func encrypt(uaPublic, authSecret, salt []byte, as *ecdh.PrivateKey, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("push payload is %d bytes; at most %d fit", len(plaintext), MaxPayload)
	}
	if len(authSecret) != 16 || len(salt) != 16 {
		return nil, errors.New("auth secret and salt must be 16 bytes")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	shared, err := as.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	const rs = 4096
	header := make([]byte, 0, 16+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, rs)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	record := append(append([]byte{}, plaintext...), 0x02) // last record, no padding
	return gcm.Seal(header, nonce, record, nil), nil
}

// Sender posts messages to push services.
type Sender struct {
	Key *ecdsa.PrivateKey
	// Subject is how the push service can reach the sender: the server's
	// https URL or a mailto: address.
	Subject string
	// Client sends the requests; nil uses PublicClient.
	Client *http.Client
	Now    func() time.Time
}

// Message is one notification.
type Message struct {
	Payload []byte
	TTL     time.Duration // how long the push service keeps it for an offline device
	Topic   string        // a newer message with the same topic replaces an undelivered one (optional, base64url, <= 32 chars)
	Urgent  bool
}

// Send encrypts m for sub and posts it. It returns ErrGone when the
// subscription is no more.
func (s *Sender) Send(ctx context.Context, sub Subscription, m Message) error {
	if err := CheckEndpoint(sub.Endpoint, s.Client != nil); err != nil {
		return err
	}
	body, err := Encrypt(sub, m.Payload)
	if err != nil {
		return err
	}
	auth, err := s.authorization(sub.Endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(m.TTL/time.Second)))
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}
	if m.Urgent {
		req.Header.Set("Urgency", "high")
	}
	c := s.Client
	if c == nil {
		c = PublicClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	}
	return fmt.Errorf("push service answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

// CheckEndpoint refuses endpoints that are not https URLs; custom is set
// when the caller brought its own client (tests), which may use http.
func CheckEndpoint(endpoint string, custom bool) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(custom && u.Scheme == "http")) {
		return errors.New("the push endpoint must be an https URL")
	}
	return nil
}

// authorization is the VAPID header for the endpoint's push service: a JWT
// for its origin, valid 12 hours, and the public key.
func (s *Sender) authorization(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	pub, err := PublicKey(s.Key)
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now().Add(12 * time.Hour).Unix(),
		"sub": s.Subject,
	})
	if err != nil {
		return "", err
	}
	signing := b64([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(rand.Reader, s.Key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64(sig) + ", k=" + pub, nil
}

// PublicClient only connects to public addresses: a subscription names any
// URL, and the server must not post to its own network on a person's
// say-so.
var PublicClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("push endpoint resolves to %s, which is not a public address", host)
	}
	return nil
}
