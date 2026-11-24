package blob

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// S3Config says how to reach a bucket of an S3-compatible service: AWS S3,
// Cloudflare R2, MinIO, or Google Cloud Storage through its XML API with
// HMAC keys.
type S3Config struct {
	Bucket string
	// Prefix is prepended to every key; "" or ending in "/".
	Prefix string
	// Region signs requests: "us-east-1" for MinIO, "auto" for R2 and GCS.
	Region string
	// Endpoint is scheme://host[:port]; empty means AWS S3 in Region.
	Endpoint string
	// PathStyle puts the bucket in the path (endpoint/bucket/key) instead
	// of the host name (bucket.endpoint/key).
	PathStyle   bool
	Credentials Credentials
	// HTTPClient defaults to one with sensible timeouts.
	HTTPClient *http.Client
	// MaxAttempts bounds tries per request, retrying network errors, 5xx
	// and 429 with exponential backoff (default 5).
	MaxAttempts int
	// RetryBase is the first backoff (default 200ms); it doubles per try,
	// up to 5 s, with jitter.
	RetryBase time.Duration
}

// S3Client is a small client for the part of the S3 API vx needs: put,
// get, head, list and delete objects, and multipart uploads for large
// streams. Every request is signed with SigV4 and retried with backoff.
type S3Client struct {
	cfg  S3Config
	base *url.URL
	hc   *http.Client
	now  func() time.Time
}

// NewS3Client checks cfg and returns a client.
func NewS3Client(cfg S3Config) (*S3Client, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("object storage: no bucket")
	}
	if !cfg.Credentials.valid() {
		return nil, errors.New("object storage: no access key id or secret access key")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	cfg.Prefix = strings.TrimLeft(cfg.Prefix, "/")
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 200 * time.Millisecond
	}
	ep := cfg.Endpoint
	if ep == "" {
		ep = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	base, err := url.Parse(ep)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("object storage: endpoint %q is not an http(s) URL", cfg.Endpoint)
	}
	if base.User != nil {
		return nil, errors.New("object storage: put credentials in the environment, not in the endpoint URL")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = 64
		tr.ResponseHeaderTimeout = 60 * time.Second
		hc = &http.Client{Transport: tr}
	}
	return &S3Client{cfg: cfg, base: base, hc: hc, now: time.Now}, nil
}

// Prefix is the key prefix of every object, "" or ending in "/".
func (c *S3Client) Prefix() string { return c.cfg.Prefix }

// String names the bucket for logs, without credentials.
func (c *S3Client) String() string {
	return "s3://" + c.cfg.Bucket + "/" + c.cfg.Prefix + " at " + c.base.Host
}

func (c *S3Client) url(key string, q url.Values) *url.URL {
	u := *c.base
	var p string
	if c.cfg.PathStyle {
		p = "/" + c.cfg.Bucket + "/" + key
	} else {
		u.Host = c.cfg.Bucket + "." + u.Host
		p = "/" + key
	}
	if key == "" {
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			p = "/"
		}
	}
	u.Path = strings.TrimSuffix(c.base.Path, "/") + p
	u.RawPath = uriEncode(u.Path, false)
	u.RawQuery = ""
	if len(q) > 0 {
		u.RawQuery = canonicalQuery(q)
	}
	return &u
}

// S3Error is an error response from the service.
type S3Error struct {
	Method, Key string
	Status      int
	Code        string
	Message     string
}

func (e *S3Error) Error() string {
	msg := fmt.Sprintf("object storage %s %s: %d %s", e.Method, e.Key, e.Status, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	switch e.Status {
	case http.StatusForbidden, http.StatusUnauthorized:
		msg += " (check the access key, its permissions on the bucket and the region)"
	case http.StatusMovedPermanently, http.StatusTemporaryRedirect:
		msg += " (the bucket is in another region: set region= in the URL)"
	}
	return msg
}

// IsNotFound reports whether err is a 404 from the service.
func IsNotFound(err error) bool {
	var e *S3Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func readS3Error(method, key string, resp *http.Response) error {
	e := &S3Error{Method: method, Key: key, Status: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &x) == nil {
		e.Code, e.Message = x.Code, x.Message
	}
	if e.Code == "" {
		e.Code = http.StatusText(resp.StatusCode)
	}
	return e
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout || status == http.StatusRequestTimeout
}

func (c *S3Client) backoff(ctx context.Context, attempt int) error {
	d := c.cfg.RetryBase << attempt
	if d > 5*time.Second || d <= 0 {
		d = 5 * time.Second
	}
	d = d/2 + rand.N(d/2+1)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do sends one request, retrying network errors and retryable statuses.
// A 2xx response (and a 404 when ok404) is returned with its body open;
// anything else becomes an error.
func (c *S3Client) do(ctx context.Context, method, key string, q url.Values, body []byte, hdr http.Header, ok404 bool) (*http.Response, error) {
	payload := emptyPayloadHash
	if len(body) > 0 {
		payload = sha256Hex(body)
	}
	var last error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.backoff(ctx, attempt-1); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(key, q).String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(body))
		if len(body) == 0 {
			req.Body = http.NoBody
		}
		for k, v := range hdr {
			req.Header[k] = append([]string(nil), v...)
		}
		signV4(req, c.cfg.Credentials, c.cfg.Region, payload, c.now())
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = fmt.Errorf("object storage %s %s: %w", method, key, redactURLError(err))
			continue
		}
		switch {
		case resp.StatusCode/100 == 2, ok404 && resp.StatusCode == http.StatusNotFound:
			return resp, nil
		case retryable(resp.StatusCode):
			last = readS3Error(method, key, resp)
			resp.Body.Close()
			continue
		}
		err = readS3Error(method, key, resp)
		resp.Body.Close()
		return nil, err
	}
	return nil, fmt.Errorf("%w (gave up after %d tries)", last, c.cfg.MaxAttempts)
}

// redactURLError drops the request URL from net/http errors: it holds no
// secret, but keeps messages short.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func isTemporary(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Put stores body under key (relative to the prefix).
func (c *S3Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	hdr := http.Header{}
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	resp, err := c.do(ctx, http.MethodPut, c.cfg.Prefix+key, nil, body, hdr, false)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// Head returns the object's size; exists is false for a missing object.
func (c *S3Client) Head(ctx context.Context, key string) (size int64, exists bool, err error) {
	resp, err := c.do(ctx, http.MethodHead, c.cfg.Prefix+key, nil, nil, nil, true)
	if err != nil {
		return 0, false, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	return resp.ContentLength, true, nil
}

// GetBytes reads a whole object. A missing object is an *S3Error with
// status 404 (IsNotFound). Reads cut short are retried.
func (c *S3Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.backoff(ctx, attempt-1); err != nil {
				return nil, err
			}
		}
		resp, err := c.do(ctx, http.MethodGet, c.cfg.Prefix+key, nil, nil, nil, true)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			err := readS3Error(http.MethodGet, c.cfg.Prefix+key, resp)
			resp.Body.Close()
			return nil, err
		}
		data, err := readAllSized(resp.Body, resp.ContentLength)
		resp.Body.Close()
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = fmt.Errorf("object storage GET %s: %w", key, err)
	}
	return nil, last
}

func readAllSized(r io.Reader, n int64) ([]byte, error) {
	if n < 0 {
		return io.ReadAll(r)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Open streams an object. When the connection breaks mid-way, the reader
// continues where it stopped with a ranged request for the same version
// (If-Match on its ETag), so large downloads survive flaky networks.
func (c *S3Client) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, c.cfg.Prefix+key, nil, nil, nil, true)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		err := readS3Error(http.MethodGet, c.cfg.Prefix+key, resp)
		resp.Body.Close()
		return nil, 0, err
	}
	return &resumeReader{ctx: ctx, c: c, key: c.cfg.Prefix + key, body: resp.Body, size: resp.ContentLength,
		etag: resp.Header.Get("ETag")}, resp.ContentLength, nil
}

type resumeReader struct {
	ctx    context.Context
	c      *S3Client
	key    string
	body   io.ReadCloser
	off    int64
	size   int64
	etag   string
	resets int
}

func (r *resumeReader) Read(p []byte) (int, error) {
	for {
		n, err := r.body.Read(p)
		r.off += int64(n)
		if err == nil || n > 0 {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			if r.size >= 0 && r.off < r.size {
				err = io.ErrUnexpectedEOF
			} else {
				return 0, io.EOF
			}
		}
		if !isTemporary(err) || r.resets >= r.c.cfg.MaxAttempts || r.etag == "" || r.ctx.Err() != nil {
			return 0, err
		}
		r.resets++
		r.body.Close()
		hdr := http.Header{}
		hdr.Set("Range", "bytes="+strconv.FormatInt(r.off, 10)+"-")
		hdr.Set("If-Match", r.etag)
		resp, derr := r.c.do(r.ctx, http.MethodGet, r.key, nil, nil, hdr, false)
		if derr != nil {
			r.body = io.NopCloser(bytes.NewReader(nil))
			return 0, fmt.Errorf("%w (resuming at byte %d: %v)", err, r.off, derr)
		}
		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			r.body = io.NopCloser(bytes.NewReader(nil))
			return 0, fmt.Errorf("%w (the service does not resume downloads)", err)
		}
		r.body = resp.Body
	}
}

func (r *resumeReader) Close() error { return r.body.Close() }

// Delete removes an object; a missing object is not an error.
func (c *S3Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.cfg.Prefix+key, nil, nil, nil, true)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ListEntry is one object of a listing; Key is relative to the prefix.
type ListEntry struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List calls fn for every object whose key (relative to the client's
// prefix) starts with prefix, in key order. It uses ListObjects (v1) with
// markers, which every S3-compatible service supports.
func (c *S3Client) List(ctx context.Context, prefix string, fn func(ListEntry) error) error {
	marker := ""
	full := c.cfg.Prefix + prefix
	for {
		q := url.Values{"prefix": {full}, "max-keys": {"1000"}}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, nil, false)
		if err != nil {
			return err
		}
		var page struct {
			IsTruncated bool   `xml:"IsTruncated"`
			NextMarker  string `xml:"NextMarker"`
			Contents    []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("object storage list %s: %w", full, err)
		}
		for _, o := range page.Contents {
			if !strings.HasPrefix(o.Key, c.cfg.Prefix) {
				continue
			}
			if err := fn(ListEntry{Key: strings.TrimPrefix(o.Key, c.cfg.Prefix), Size: o.Size, LastModified: o.LastModified}); err != nil {
				return err
			}
		}
		if !page.IsTruncated || len(page.Contents) == 0 {
			return nil
		}
		marker = page.NextMarker
		if marker == "" {
			marker = page.Contents[len(page.Contents)-1].Key
		}
	}
}

// MinPartSize is the smallest part S3 accepts in a multipart upload.
const MinPartSize = 5 << 20

// PutStream uploads r under key with bounded memory: a body that fits in
// one part goes up with one PUT, a larger one as a multipart upload of
// partSize parts (default 16 MB, at least 5 MB). A failed upload is aborted
// so it leaves nothing behind. It returns the bytes uploaded.
func (c *S3Client) PutStream(ctx context.Context, key string, r io.Reader, partSize int, contentType string) (int64, error) {
	if partSize <= 0 {
		partSize = 16 << 20
	}
	partSize = max(partSize, MinPartSize)
	buf := make([]byte, partSize)
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return int64(n), c.Put(ctx, key, buf[:n], contentType)
	}
	if err != nil {
		return 0, err
	}
	full := c.cfg.Prefix + key
	hdr := http.Header{}
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	resp, err := c.do(ctx, http.MethodPost, full, url.Values{"uploads": {""}}, nil, hdr, false)
	if err != nil {
		return 0, err
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()
	if err != nil || init.UploadID == "" {
		return 0, fmt.Errorf("object storage: start multipart upload of %s: %v", key, err)
	}
	abort := func(cause error) (int64, error) {
		// A fresh context: the upload is aborted even when ctx was cancelled.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if r, err := c.do(actx, http.MethodDelete, full, url.Values{"uploadId": {init.UploadID}}, nil, nil, true); err == nil {
			r.Body.Close()
		}
		return 0, cause
	}
	type part struct {
		XMLName    xml.Name `xml:"Part"`
		PartNumber int      `xml:"PartNumber"`
		ETag       string   `xml:"ETag"`
	}
	var parts []part
	var total int64
	for num := 1; n > 0; num++ {
		q := url.Values{"partNumber": {strconv.Itoa(num)}, "uploadId": {init.UploadID}}
		resp, err := c.do(ctx, http.MethodPut, full, q, buf[:n], nil, false)
		if err != nil {
			return abort(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		parts = append(parts, part{PartNumber: num, ETag: resp.Header.Get("ETag")})
		total += int64(n)
		n, err = io.ReadFull(r, buf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return abort(err)
		}
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		return abort(err)
	}
	resp, err = c.do(ctx, http.MethodPost, full, url.Values{"uploadId": {init.UploadID}}, body, http.Header{"Content-Type": {"application/xml"}}, false)
	if err != nil {
		return abort(err)
	}
	// S3 can report a failed completion inside a 200 response.
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if bytes.Contains(reply, []byte("<Error>")) {
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		_ = xml.Unmarshal(reply, &x)
		return abort(&S3Error{Method: "POST", Key: full, Status: http.StatusOK, Code: x.Code, Message: x.Message})
	}
	return total, nil
}

// CreateBucket creates the client's bucket; one that exists and is yours
// is not an error. Servers never call it (buckets are made by whoever runs
// the service); tests and first-time setup against MinIO do.
func (c *S3Client) CreateBucket(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPut, "", nil, nil, nil, false)
	if err != nil {
		var e *S3Error
		if errors.As(err, &e) && e.Status == http.StatusConflict && e.Code == "BucketAlreadyOwnedByYou" {
			return nil
		}
		return err
	}
	return resp.Body.Close()
}
