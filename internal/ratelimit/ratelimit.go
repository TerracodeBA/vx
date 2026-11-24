// Package ratelimit keeps token buckets per class (API, sign-in, sign-up,
// CLI login, webhooks) and key (a token or a client address), in memory.
// Each server process counts on its own: behind a load balancer with N
// replicas, a client gets up to N times the configured rate.
package ratelimit

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Class is what a bucket limits.
type Class string

const (
	APIToken Class = "api_token" // API and MCP calls with one token
	APIIP    Class = "api_ip"    // API and MCP calls from one address, with or without a token
	SignIn   Class = "signin"    // sign-in pages and callbacks, per address
	SignUp   Class = "signup"    // new accounts, per address
	CLILogin Class = "cli_login" // vx login started or approved, per address
	Webhook  Class = "webhook"   // webhook deliveries, per address
)

// Classes lists every class, in the order the docs and config show them.
var Classes = []Class{APIToken, APIIP, SignIn, SignUp, CLILogin, Webhook}

// Rate is a token bucket: Burst requests at once, refilled at PerSecond.
// The zero Rate means no limit.
type Rate struct {
	PerSecond float64
	Burst     int
}

// Off reports whether the rate limits nothing.
func (r Rate) Off() bool { return r.PerSecond <= 0 }

// ParseRate reads "N/s", "N/m" or "N/h", optionally followed by
// ",burst=B", or "off". Examples: "20/s,burst=200", "30/m", "off".
// Without burst=, the burst is N: a client may use a whole period's
// requests at once, then waits for the refill.
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "off" || s == "0" || s == "none" || s == "unlimited" {
		return Rate{}, nil
	}
	main, rest, _ := strings.Cut(s, ",")
	num, unit, ok := strings.Cut(main, "/")
	if !ok {
		return Rate{}, fmt.Errorf("rate %q: want N/s, N/m or N/h (like 20/s or 30/m,burst=10), or off", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || n <= 0 || math.IsInf(n, 0) {
		return Rate{}, fmt.Errorf("rate %q: %q is not a positive number", s, num)
	}
	var per float64
	switch strings.TrimSpace(unit) {
	case "s", "sec", "second":
		per = 1
	case "m", "min", "minute":
		per = 60
	case "h", "hour":
		per = 3600
	default:
		return Rate{}, fmt.Errorf("rate %q: unit %q must be s, m or h", s, unit)
	}
	r := Rate{PerSecond: n / per, Burst: int(math.Ceil(n))}
	if rest != "" {
		k, v, ok := strings.Cut(strings.TrimSpace(rest), "=")
		if !ok || strings.TrimSpace(k) != "burst" {
			return Rate{}, fmt.Errorf("rate %q: after the comma write burst=N", s)
		}
		b, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || b < 1 {
			return Rate{}, fmt.Errorf("rate %q: burst must be a whole number of at least 1", s)
		}
		r.Burst = b
	}
	return r, nil
}

// String is the rate in ParseRate's format.
func (r Rate) String() string {
	if r.Off() {
		return "off"
	}
	n, unit := r.PerSecond, "s"
	switch {
	case n >= 1:
	case n*60 >= 1:
		n, unit = n*60, "m"
	default:
		n, unit = n*3600, "h"
	}
	return strconv.FormatFloat(math.Round(n*1000)/1000, 'f', -1, 64) + "/" + unit + ",burst=" + strconv.Itoa(r.Burst)
}

// Config is the rate per class. Missing classes are not limited.
type Config map[Class]Rate

// Defaults are generous for a team, and stop one client from exhausting a
// shared server. An agent landing or syncing makes a few dozen API calls.
func Defaults() Config {
	return Config{
		APIToken: {PerSecond: 30, Burst: 300},
		APIIP:    {PerSecond: 100, Burst: 1000},
		SignIn:   {PerSecond: 30.0 / 60, Burst: 30},
		SignUp:   {PerSecond: 10.0 / 3600, Burst: 5},
		CLILogin: {PerSecond: 10.0 / 60, Burst: 10},
		Webhook:  {PerSecond: 20, Burst: 200},
	}
}

// EnvName is the server environment variable that sets class's rate.
func EnvName(c Class) string { return "VX_RATE_" + strings.ToUpper(string(c)) }

// FromEnv is Defaults with each VX_RATE_<CLASS> that getenv returns
// applied, and VX_RATE_LIMITS=off turning every limit off.
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Defaults()
	if v := strings.TrimSpace(strings.ToLower(getenv("VX_RATE_LIMITS"))); v == "off" || v == "false" || v == "0" {
		return Config{}, nil
	}
	for _, c := range Classes {
		v := getenv(EnvName(c))
		if v == "" {
			continue
		}
		r, err := ParseRate(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvName(c), err)
		}
		cfg[c] = r
	}
	return cfg, nil
}

// maxKeys bounds the buckets of one class. When a class has more, idle
// buckets are dropped first; if all are busy the class starts over, which
// briefly lets everyone through rather than refusing everyone.
const maxKeys = 100_000

// Limiter is a set of token buckets. The zero value and nil limit nothing.
type Limiter struct {
	cfg Config
	Now func() time.Time

	mu        sync.Mutex
	buckets   map[Class]map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New returns a limiter for cfg.
func New(cfg Config) *Limiter {
	return &Limiter{cfg: cfg, Now: time.Now, buckets: map[Class]map[string]*bucket{}}
}

// Config returns the limiter's rates.
func (l *Limiter) Config() Config {
	if l == nil {
		return nil
	}
	return l.cfg
}

// Allow takes one request from key's bucket of class. When it is empty,
// Allow returns false and how long until one request is allowed again.
func (l *Limiter) Allow(c Class, key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	r, ok := l.cfg[c]
	if !ok || r.Off() {
		return true, 0
	}
	burst := float64(max(r.Burst, 1))
	now := l.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	m := l.buckets[c]
	if m == nil {
		m = map[string]*bucket{}
		l.buckets[c] = m
	}
	b := m[key]
	if b == nil {
		if len(m) >= maxKeys {
			l.sweepClass(c, now)
			if m = l.buckets[c]; len(m) >= maxKeys {
				m = map[string]*bucket{}
				l.buckets[c] = m
			}
		}
		b = &bucket{tokens: burst, at: now}
		m[key] = b
	}
	if el := now.Sub(b.at).Seconds(); el > 0 {
		b.tokens = math.Min(burst, b.tokens+el*r.PerSecond)
		b.at = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / r.PerSecond * float64(time.Second))
	return false, max(wait, time.Millisecond)
}

// sweep drops full buckets once a minute: a full bucket is the same as
// none.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for c := range l.buckets {
		l.sweepClass(c, now)
	}
}

func (l *Limiter) sweepClass(c Class, now time.Time) {
	r := l.cfg[c]
	burst := float64(max(r.Burst, 1))
	for k, b := range l.buckets[c] {
		if b.tokens+now.Sub(b.at).Seconds()*r.PerSecond >= burst {
			delete(l.buckets[c], k)
		}
	}
}

// RetryText is the "retry in N s" part of a refusal: whole seconds,
// rounded up, at least 1.
func RetryText(d time.Duration) string {
	return "retry in " + strconv.Itoa(RetrySeconds(d)) + " s"
}

// RetrySeconds is d in whole seconds, rounded up, at least 1: the value of
// a Retry-After header.
func RetrySeconds(d time.Duration) int {
	return max(int(math.Ceil(d.Seconds())), 1)
}
