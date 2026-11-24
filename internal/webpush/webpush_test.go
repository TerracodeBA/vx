package webpush

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The example of RFC 8291 appendix A.
func TestWebPushEncryptRFC8291(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustDecode(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b64(asPriv.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as_public = %s", got)
	}
	ua := mustDecode(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	out, err := encrypt(ua, mustDecode(t, "BTBZMqHH6r4Tts7J_aSIgg"), mustDecode(t, "DGv6ra1nlYgDCS1FRnbzlw"), asPriv, []byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := b64(out); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

// decryptFor is the browser's side: it opens a message with the
// subscription's private key and auth secret.
func decryptFor(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	if rs := binary.BigEndian.Uint32(body[16:20]); rs != 4096 {
		t.Fatalf("rs = %d", rs)
	}
	asPub, ct := body[21:21+idlen], body[21+idlen:]
	k, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := ua.ECDH(k)
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub...)
	ikm, _ := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pt[len(pt)-1] != 0x02 {
		t.Fatalf("padding delimiter = %x", pt[len(pt)-1])
	}
	return pt[:len(pt)-1]
}

func TestWebPushSend(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := KeyBytes(key)
	again, err := ParseKey(raw)
	if err != nil || !again.Equal(key) {
		t.Fatalf("key does not round trip: %v", err)
	}
	ua, _ := ecdh.P256().GenerateKey(nil)
	auth := []byte("0123456789abcdef")
	status := http.StatusCreated
	var got []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		hdr = r.Header
		w.WriteHeader(status)
	}))
	defer srv.Close()
	sub := Subscription{Endpoint: srv.URL + "/push/abc", P256dh: b64(ua.PublicKey().Bytes()), Auth: b64(auth)}
	s := &Sender{Key: key, Subject: "https://vx.example", Client: srv.Client(), Now: func() time.Time { return time.Unix(1700000000, 0) }}
	if err := s.Send(context.Background(), sub, Message{Payload: []byte(`{"title":"hi"}`), TTL: time.Hour, Urgent: true, Topic: "q1"}); err != nil {
		t.Fatal(err)
	}
	if pt := decryptFor(t, ua, auth, got); string(pt) != `{"title":"hi"}` {
		t.Fatalf("payload = %q", pt)
	}
	if hdr.Get("Content-Encoding") != "aes128gcm" || hdr.Get("TTL") != "3600" || hdr.Get("Urgency") != "high" || hdr.Get("Topic") != "q1" {
		t.Fatalf("headers = %v", hdr)
	}
	// The VAPID JWT: signed by the key, for the push service's origin.
	a := hdr.Get("Authorization")
	pub, _ := PublicKey(key)
	if !strings.HasPrefix(a, "vapid t=") || !strings.HasSuffix(a, ", k="+pub) {
		t.Fatalf("authorization = %s", a)
	}
	jwt := strings.TrimSuffix(strings.TrimPrefix(a, "vapid t="), ", k="+pub)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %s", jwt)
	}
	var claims map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(cb, &claims)
	if claims["aud"] != srv.URL || claims["sub"] != "https://vx.example" || claims["exp"].(float64) != 1700000000+12*3600 {
		t.Fatalf("claims = %v", claims)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, d[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("bad VAPID signature")
	}
	status = http.StatusGone
	if err := s.Send(context.Background(), sub, Message{Payload: []byte("x")}); !errors.Is(err, ErrGone) {
		t.Fatalf("410 = %v", err)
	}
	status = http.StatusTooManyRequests
	if err := s.Send(context.Background(), sub, Message{Payload: []byte("x")}); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("429 = %v", err)
	}
	if err := s.Send(context.Background(), sub, Message{Payload: make([]byte, MaxPayload+1)}); err == nil {
		t.Fatal("oversized payload sent")
	}
}

func TestWebPushCheckSubscription(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(nil)
	ok := Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/abc", P256dh: b64(ua.PublicKey().Bytes()), Auth: b64(make([]byte, 16))}
	if err := CheckSubscription(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Subscription){
		"http":       func(s *Subscription) { s.Endpoint = "http://fcm.googleapis.com/x" },
		"ip":         func(s *Subscription) { s.Endpoint = "https://169.254.169.254/x" },
		"localhost":  func(s *Subscription) { s.Endpoint = "https://localhost/x" },
		"bare host":  func(s *Subscription) { s.Endpoint = "https://metadata/x" },
		"internal":   func(s *Subscription) { s.Endpoint = "https://metadata.google.internal/x" },
		"userinfo":   func(s *Subscription) { s.Endpoint = "https://a:b@fcm.googleapis.com/x" },
		"bad key":    func(s *Subscription) { s.P256dh = b64([]byte("nope")) },
		"short auth": func(s *Subscription) { s.Auth = b64(make([]byte, 8)) },
	} {
		s := ok
		mut(&s)
		if err := CheckSubscription(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The default client refuses private addresses even when DNS points there.
	err := (&Sender{Key: mustKey(t), Subject: "https://vx.example"}).Send(context.Background(), Subscription{Endpoint: "https://127.0.0.1:1/x", P256dh: ok.P256dh, Auth: ok.Auth}, Message{Payload: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("private address = %v", err)
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
