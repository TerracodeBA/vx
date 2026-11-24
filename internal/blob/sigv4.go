package blob

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Credentials sign requests to an S3-compatible service. They never appear
// in logs or error messages: String, GoString and LogValue redact them.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional, for temporary credentials
}

const redacted = "[redacted credentials]"

func (Credentials) String() string       { return redacted }
func (Credentials) GoString() string     { return redacted }
func (Credentials) LogValue() slog.Value { return slog.StringValue(redacted) }

// Format keeps %v, %+v and %#v of a struct holding Credentials redacted.
func (Credentials) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

func (c Credentials) valid() bool { return c.AccessKeyID != "" && c.SecretAccessKey != "" }

// emptyPayloadHash is the SHA-256 of no bytes.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// signV4 signs req with AWS Signature Version 4 for the service "s3".
// payloadHash is the hex SHA-256 of the body. It sets X-Amz-Date,
// X-Amz-Content-Sha256, X-Amz-Security-Token (temporary credentials) and
// Authorization. Signed are Host, every X-Amz-* header and Content-Type,
// Content-MD5 and Range when present.
func signV4(req *http.Request, cr Credentials, region, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if cr.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", cr.SessionToken)
	}
	canonical, signed := canonicalRequest(req, payloadHash)
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+cr.SecretAccessKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cr.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

// canonicalRequest returns the SigV4 canonical request and the signed
// header list.
func canonicalRequest(req *http.Request, payloadHash string) (string, string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, vals := range req.Header {
		l := strings.ToLower(name)
		if strings.HasPrefix(l, "x-amz-") || l == "content-type" || l == "content-md5" || l == "range" {
			for i := range vals {
				vals[i] = strings.Join(strings.Fields(vals[i]), " ")
			}
			headers[l] = strings.Join(vals, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + headers[n] + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return req.Method + "\n" + path + "\n" + canonicalQuery(req.URL.Query()) + "\n" + ch.String() + "\n" +
		signed + "\n" + payloadHash, signed
}

func canonicalQuery(q map[string][]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes everything but unreserved characters (and '/'
// unless encodeSlash), as SigV4 requires.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

// VerifySigV4 checks a signed request the way a server does, recomputing
// the signature from what arrived. Test servers (internal/blob/s3test) use
// it; body is the request body already read.
func VerifySigV4(r *http.Request, cr Credentials, region string, body []byte) error {
	auth := r.Header.Get("Authorization")
	const algo = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, algo) {
		return fmt.Errorf("not a SigV4 request")
	}
	fields := map[string]string{}
	for _, f := range strings.Split(strings.TrimPrefix(auth, algo), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[0] != cr.AccessKeyID || cred[2] != region || cred[3] != "s3" {
		return fmt.Errorf("wrong credential scope %q", fields["Credential"])
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload != sha256Hex(body) {
		return fmt.Errorf("payload hash does not match the body")
	}
	t, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return fmt.Errorf("bad X-Amz-Date: %v", err)
	}
	// Recompute over exactly the headers the client signed.
	signedNames := strings.Split(fields["SignedHeaders"], ";")
	clone := r.Clone(r.Context())
	clone.Header = http.Header{}
	for _, n := range signedNames {
		if n == "host" {
			continue
		}
		if v, ok := r.Header[http.CanonicalHeaderKey(n)]; ok {
			clone.Header[http.CanonicalHeaderKey(n)] = append([]string(nil), v...)
		}
	}
	clone.Header.Del("Authorization")
	clone.Header.Del("X-Amz-Date")
	clone.Header.Del("X-Amz-Content-Sha256")
	clone.Header.Del("X-Amz-Security-Token")
	if clone.Host == "" {
		clone.Host = r.Host
	}
	signV4(clone, cr, region, payload, t)
	if r.Header.Get("X-Amz-Security-Token") != cr.SessionToken {
		return fmt.Errorf("wrong session token")
	}
	if clone.Header.Get("Authorization") != auth {
		canonical, _ := canonicalRequest(clone, payload)
		return fmt.Errorf("signature does not match; canonical request:\n%s", canonical)
	}
	return nil
}
