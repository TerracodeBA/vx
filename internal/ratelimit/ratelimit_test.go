package ratelimit

import (
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Rate
		str  string
	}{
		{"20/s,burst=200", Rate{PerSecond: 20, Burst: 200}, "20/s,burst=200"},
		{"30/m", Rate{PerSecond: 0.5, Burst: 30}, "30/m,burst=30"},
		{"10/h, burst=5", Rate{PerSecond: 10.0 / 3600, Burst: 5}, "10/h,burst=5"},
		{"off", Rate{}, "off"},
		{"0", Rate{}, "off"},
	} {
		got, err := ParseRate(tc.in)
		if err != nil {
			t.Fatalf("ParseRate(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseRate(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.str {
			t.Errorf("%q.String() = %q, want %q", tc.in, got.String(), tc.str)
		}
	}
	for _, bad := range []string{"20", "x/s", "-1/s", "5/d", "5/s,b=3", "5/s,burst=0"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) accepted", bad)
		}
	}
}

func TestLimiterTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(Config{SignIn: {PerSecond: 1, Burst: 3}})
	l.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(SignIn, "1.2.3.4"); !ok {
			t.Fatalf("request %d refused within the burst", i+1)
		}
	}
	ok, wait := l.Allow(SignIn, "1.2.3.4")
	if ok || wait != time.Second {
		t.Fatalf("4th request: ok=%v wait=%v, want refused for 1s", ok, wait)
	}
	if ok, _ := l.Allow(SignIn, "5.6.7.8"); !ok {
		t.Fatal("another address shares the bucket")
	}
	if ok, _ := l.Allow(APIToken, "tok"); !ok {
		t.Fatal("a class without a rate is limited")
	}
	now = now.Add(1500 * time.Millisecond)
	if ok, _ := l.Allow(SignIn, "1.2.3.4"); !ok {
		t.Fatal("refill did not allow a request after 1.5s")
	}
	ok, wait = l.Allow(SignIn, "1.2.3.4")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("after refill: ok=%v wait=%v, want refused for 0.5s", ok, wait)
	}
	if RetryText(wait) != "retry in 1 s" || RetrySeconds(2100*time.Millisecond) != 3 {
		t.Fatalf("RetryText = %q", RetryText(wait))
	}
	// Idle full buckets are swept.
	now = now.Add(time.Hour)
	l.Allow(SignIn, "9.9.9.9")
	if n := len(l.buckets[SignIn]); n != 1 {
		t.Fatalf("buckets after sweep = %d, want 1", n)
	}
	var none *Limiter
	if ok, _ := none.Allow(SignIn, "x"); !ok {
		t.Fatal("nil limiter limits")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"VX_RATE_SIGNIN": "5/m", "VX_RATE_WEBHOOK": "off"}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg[SignIn] != (Rate{PerSecond: 5.0 / 60, Burst: 5}) || !cfg[Webhook].Off() || cfg[APIToken] != Defaults()[APIToken] {
		t.Fatalf("cfg = %+v", cfg)
	}
	env["VX_RATE_API_TOKEN"] = "lots"
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("bad rate accepted")
	}
	cfg, _ = FromEnv(func(k string) string { return map[string]string{"VX_RATE_LIMITS": "off"}[k] })
	if len(cfg) != 0 {
		t.Fatalf("VX_RATE_LIMITS=off left %v", cfg)
	}
}
