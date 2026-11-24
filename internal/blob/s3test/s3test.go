// Package s3test is an in-process fake of the S3 API for tests: one bucket,
// objects in memory, SigV4 signatures checked on every request, multipart
// uploads, ranged reads, ListObjects (v1) and injected failures. It speaks
// path style and virtual-host style (bucket.HOST).
package s3test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
)

// Server is a running fake.
type Server struct {
	*httptest.Server
	Bucket string
	Region string
	Creds  blob.Credentials

	mu       sync.Mutex
	objects  map[string][]byte
	modified map[string]time.Time
	uploads  map[string]map[int][]byte
	nextID   int
	failures []int // statuses to answer the next requests with
	counts   map[string]int
	// PartialBodies cuts the next n GET bodies short after half the bytes.
	partial int
}

// New starts a fake with bucket "vx-test" and test credentials; it stops
// when the test ends.
func New(t testing.TB) *Server {
	s := &Server{
		Bucket: "vx-test", Region: "us-east-1",
		Creds:   blob.Credentials{AccessKeyID: "AKIATESTFAKE", SecretAccessKey: "fake-secret-key-for-tests"},
		objects: map[string][]byte{}, modified: map[string]time.Time{}, uploads: map[string]map[int][]byte{}, counts: map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// BucketURL is the vx bucket URL for this fake with prefix, e.g.
// s3://vx-test/prefix?endpoint=http://127.0.0.1:PORT.
func (s *Server) BucketURL(prefix string) string {
	return "s3://" + s.Bucket + "/" + prefix + "?endpoint=" + s.Server.URL
}

// Env returns a getenv for blob.OpenOptions with the fake's credentials
// under the prefix (VX_BLOB by default).
func (s *Server) Env(prefix string) func(string) string {
	if prefix == "" {
		prefix = "VX_BLOB"
	}
	return func(k string) string {
		switch k {
		case prefix + "_ACCESS_KEY_ID":
			return s.Creds.AccessKeyID
		case prefix + "_SECRET_ACCESS_KEY":
			return s.Creds.SecretAccessKey
		}
		return ""
	}
}

// FailNext answers the next len(statuses) requests with those statuses.
func (s *Server) FailNext(statuses ...int) {
	s.mu.Lock()
	s.failures = append(s.failures, statuses...)
	s.mu.Unlock()
}

// CutNextBodies makes the next n GET responses stop after half the body,
// as a broken connection would.
func (s *Server) CutNextBodies(n int) {
	s.mu.Lock()
	s.partial += n
	s.mu.Unlock()
}

// Count returns how many requests of a method arrived (after failures).
func (s *Server) Count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[method]
}

// Keys lists stored keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Object returns a stored object's bytes.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

// SetObject stores bytes directly, for corruption tests.
func (s *Server) SetObject(key string, b []byte) {
	s.mu.Lock()
	s.objects[key] = b
	s.modified[key] = time.Now()
	s.mu.Unlock()
}

// SetModified changes when an object was last written, for retention tests.
func (s *Server) SetModified(key string, t time.Time) {
	s.mu.Lock()
	s.modified[key] = t
	s.mu.Unlock()
}

// OpenUploads is how many multipart uploads were started and neither
// completed nor aborted.
func (s *Server) OpenUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

func xmlError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

func etag(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		xmlError(w, 400, "IncompleteBody", err.Error())
		return
	}
	s.mu.Lock()
	if len(s.failures) > 0 {
		st := s.failures[0]
		s.failures = s.failures[1:]
		s.mu.Unlock()
		xmlError(w, st, "InjectedFailure", "injected by the test")
		return
	}
	s.counts[r.Method]++
	s.mu.Unlock()
	if err := blob.VerifySigV4(r, s.Creds, s.Region, body); err != nil {
		xmlError(w, 403, "SignatureDoesNotMatch", err.Error())
		return
	}
	// Bucket from the host (virtual style) or the first path element.
	path := r.URL.Path
	var bucket, key string
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if strings.HasPrefix(host, s.Bucket+".") {
		bucket, key = s.Bucket, strings.TrimPrefix(path, "/")
	} else {
		rest := strings.TrimPrefix(path, "/")
		bucket, key, _ = strings.Cut(rest, "/")
	}
	if bucket != s.Bucket {
		xmlError(w, 404, "NoSuchBucket", "no bucket "+bucket)
		return
	}
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case key == "" && r.Method == http.MethodGet:
		s.list(w, q.Get("prefix"), q.Get("marker"), q.Get("max-keys"))
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.nextID++
		id := "upload-" + strconv.Itoa(s.nextID)
		s.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", bucket, key, id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		parts, ok := s.uploads[q.Get("uploadId")]
		if !ok {
			xmlError(w, 404, "NoSuchUpload", "")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts[n] = body
		w.Header().Set("ETag", etag(body))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts, ok := s.uploads[q.Get("uploadId")]
		if !ok {
			xmlError(w, 404, "NoSuchUpload", "")
			return
		}
		var done struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &done); err != nil {
			xmlError(w, 400, "MalformedXML", err.Error())
			return
		}
		var all bytes.Buffer
		for i, p := range done.Parts {
			data, ok := parts[p.PartNumber]
			if !ok || p.PartNumber != i+1 || etag(data) != p.ETag {
				xmlError(w, 400, "InvalidPart", fmt.Sprintf("part %d", p.PartNumber))
				return
			}
			if i < len(done.Parts)-1 && len(data) < blob.MinPartSize {
				xmlError(w, 400, "EntityTooSmall", fmt.Sprintf("part %d", p.PartNumber))
				return
			}
			all.Write(data)
		}
		s.objects[key] = all.Bytes()
		s.modified[key] = time.Now()
		delete(s.uploads, q.Get("uploadId"))
		fmt.Fprintf(w, "<CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>", key)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		s.objects[key] = body
		s.modified[key] = time.Now()
		w.Header().Set("ETag", etag(body))
	case r.Method == http.MethodHead, r.Method == http.MethodGet:
		data, ok := s.objects[key]
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(404)
				return
			}
			xmlError(w, 404, "NoSuchKey", "The specified key does not exist.")
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && m != etag(data) {
			xmlError(w, 412, "PreconditionFailed", "")
			return
		}
		w.Header().Set("ETag", etag(data))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		status := http.StatusOK
		if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			if from > len(data) {
				from = len(data)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
			data, status = data[from:], http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		if s.partial > 0 && len(data) > 1 {
			s.partial--
			w.Write(data[:len(data)/2])
			// Break the connection so the client sees a short body.
			if hj, ok := w.(http.Hijacker); ok {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		delete(s.modified, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		xmlError(w, 405, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) list(w http.ResponseWriter, prefix, marker, maxKeys string) {
	limit, _ := strconv.Atoi(maxKeys)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	// Small pages in tests exercise pagination.
	limit = min(limit, 7)
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > marker {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > limit
	if truncated {
		keys = keys[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><IsTruncated>%t</IsTruncated>", s.Bucket, prefix, truncated)
	for _, k := range keys {
		var esc bytes.Buffer
		_ = xml.EscapeText(&esc, []byte(k))
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>", esc.String(), len(s.objects[k]), s.modified[k].UTC().Format(time.RFC3339))
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, b.String())
}
