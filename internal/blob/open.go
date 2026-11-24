package blob

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultCacheBytes is the memory cache in front of a bucket unless the URL
// says otherwise (cache=).
const DefaultCacheBytes = 64 << 20

// OpenOptions configures Open.
type OpenOptions struct {
	// DiskDir is the disk store used when the URL is empty.
	DiskDir string
	// EnvPrefix names the credential variables: PREFIX_ACCESS_KEY_ID,
	// PREFIX_SECRET_ACCESS_KEY and PREFIX_SESSION_TOKEN, or the same with
	// _FILE naming a file that holds the value. Default "VX_BLOB". The
	// AWS_* variables are the fallback.
	EnvPrefix string
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// NoCache opens a bucket without the memory cache (copies verify what
	// is really stored).
	NoCache    bool
	HTTPClient *http.Client
}

// Open opens the store a URL names:
//
//	""                     the disk store in DiskDir
//	file:///srv/vx/objects a disk store
//	s3://BUCKET/PREFIX?region=REGION&endpoint=https://HOST&style=path|virtual&cache=64MB
//	gs://BUCKET/PREFIX     Google Cloud Storage with HMAC keys (S3 interoperability)
//
// A bucket store gets a memory cache of hot objects (cache=, default 64MB;
// cache=0 turns it off).
func Open(rawURL string, o OpenOptions) (Store, error) {
	if strings.TrimSpace(rawURL) == "" {
		if o.DiskDir == "" {
			return nil, errors.New("blob store: no URL and no data directory")
		}
		return NewDisk(o.DiskDir)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("blob store URL: %w", err)
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("blob store URL %q: use file:///absolute/path", rawURL)
		}
		return NewDisk(filepath.FromSlash(u.Path))
	case "s3", "gs":
	default:
		return nil, fmt.Errorf("blob store URL %q: the scheme must be s3://, gs:// or file://", rawURL)
	}
	cfg, cacheBytes, err := ParseBucketURL(rawURL)
	if err != nil {
		return nil, err
	}
	if cfg.Credentials, err = CredentialsFromEnv(o.EnvPrefix, o.Getenv); err != nil {
		return nil, err
	}
	cfg.HTTPClient = o.HTTPClient
	c, err := NewS3Client(cfg)
	if err != nil {
		return nil, err
	}
	s, err := NewS3(c)
	if err != nil {
		return nil, err
	}
	if o.NoCache || cacheBytes == 0 {
		return s, nil
	}
	return NewCached(s, cacheBytes), nil
}

// ParseBucketURL reads s3:// and gs:// URLs into a configuration without
// credentials, and the cache size they ask for.
func ParseBucketURL(rawURL string) (S3Config, int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return S3Config{}, 0, fmt.Errorf("bucket URL: %w", err)
	}
	if u.User != nil {
		return S3Config{}, 0, errors.New("bucket URL: put credentials in the environment (VX_BLOB_ACCESS_KEY_ID, VX_BLOB_SECRET_ACCESS_KEY), not in the URL")
	}
	if u.Scheme != "s3" && u.Scheme != "gs" {
		return S3Config{}, 0, fmt.Errorf("bucket URL %q: the scheme must be s3:// or gs://", rawURL)
	}
	if u.Host == "" {
		return S3Config{}, 0, fmt.Errorf("bucket URL %q names no bucket: s3://BUCKET/PREFIX", rawURL)
	}
	cfg := S3Config{Bucket: u.Host, Prefix: strings.Trim(u.Path, "/")}
	if u.Scheme == "gs" {
		cfg.Endpoint, cfg.Region, cfg.PathStyle = "https://storage.googleapis.com", "auto", true
	}
	cacheBytes := int64(DefaultCacheBytes)
	style := ""
	for k, vs := range u.Query() {
		v := vs[len(vs)-1]
		switch k {
		case "region":
			cfg.Region = v
		case "endpoint":
			if !strings.Contains(v, "://") {
				v = "https://" + v
			}
			cfg.Endpoint = strings.TrimRight(v, "/")
		case "style":
			if v != "path" && v != "virtual" {
				return S3Config{}, 0, fmt.Errorf("bucket URL: style=%s: use style=path or style=virtual", v)
			}
			style = v
		case "cache":
			if cacheBytes, err = parseBytes(v); err != nil {
				return S3Config{}, 0, fmt.Errorf("bucket URL: cache=%s: %v", v, err)
			}
		default:
			return S3Config{}, 0, fmt.Errorf("bucket URL: unknown parameter %q (known: region, endpoint, style, cache)", k)
		}
	}
	switch style {
	case "path":
		cfg.PathStyle = true
	case "virtual":
		cfg.PathStyle = false
	default:
		// Own endpoints (MinIO, R2) default to path style: it needs no
		// wildcard DNS or certificate. AWS defaults to virtual hosts.
		if cfg.Endpoint != "" {
			cfg.PathStyle = true
		}
	}
	return cfg, cacheBytes, nil
}

// parseBytes reads sizes like 0, 512KB, 64MB, 1GB (powers of 1024).
func parseBytes(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("want a size like 64MB, or 0")
	}
	return n * mult, nil
}

// CredentialsFromEnv reads PREFIX_ACCESS_KEY_ID, PREFIX_SECRET_ACCESS_KEY
// and PREFIX_SESSION_TOKEN (each also as PREFIX_..._FILE, a file holding
// the value), falling back to AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and
// AWS_SESSION_TOKEN. Errors name the variables, never their values.
func CredentialsFromEnv(prefix string, getenv func(string) string) (Credentials, error) {
	if prefix == "" {
		prefix = "VX_BLOB"
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	read := func(name string) (string, error) {
		if v := getenv(name); v != "" {
			return strings.TrimSpace(v), nil
		}
		if f := getenv(name + "_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return "", fmt.Errorf("%s_FILE: %w", name, err)
			}
			return strings.TrimSpace(string(b)), nil
		}
		return "", nil
	}
	var c Credentials
	var err error
	for _, p := range []string{prefix, "AWS"} {
		if c.AccessKeyID, err = read(p + "_ACCESS_KEY_ID"); err != nil {
			return Credentials{}, err
		}
		if c.SecretAccessKey, err = read(p + "_SECRET_ACCESS_KEY"); err != nil {
			return Credentials{}, err
		}
		if c.SessionToken, err = read(p + "_SESSION_TOKEN"); err != nil {
			return Credentials{}, err
		}
		if c.valid() {
			return c, nil
		}
	}
	return Credentials{}, fmt.Errorf("no bucket credentials: set %s_ACCESS_KEY_ID and %s_SECRET_ACCESS_KEY (or the same with _FILE, a file holding the value)", prefix, prefix)
}

func errNoList(s Store) error {
	return fmt.Errorf("the blob store %T cannot list its objects", s)
}

func errNoSize(s Store) error {
	return fmt.Errorf("the blob store %T cannot tell object sizes", s)
}
