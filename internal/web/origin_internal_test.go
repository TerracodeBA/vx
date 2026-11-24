package web

import (
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		host, origin, referer string
		ok                    bool
	}{
		{"vx.local:7070", "http://vx.local:7070", "", true},
		{"vx.local:7070", "https://VX.local:7070", "", true},
		{"vx.local:7070", "", "http://vx.local:7070/ui/settings?x=1", true},
		{"vx.local:7070", "", "", false},
		{"vx.local:7070", "null", "http://vx.local:7070/ui", false},
		{"vx.local:7070", "http://vx.local:7071", "", false},
		{"vx.local:7070", "http://evil.vx.local:7070", "", false},
		{"vx.local:7070", "http://evil.example/http://vx.local:7070", "", false},
		{"vx.local:7070", "", "http://evil.example/ui?vx.local:7070", false},
		{"vx.local:7070", "", "/ui/settings", false},
		{"vx.local:7070", "javascript://vx.local:7070", "", false},
	} {
		r := httptest.NewRequest("POST", "http://"+tc.host+"/ui/logout", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.referer != "" {
			r.Header.Set("Referer", tc.referer)
		}
		if why := sameOrigin(r); (why == "") != tc.ok {
			t.Errorf("host %s Origin %q Referer %q: refused=%q, want ok=%v", tc.host, tc.origin, tc.referer, why, tc.ok)
		}
	}
}

func TestCookieName(t *testing.T) {
	for host, want := range map[string]string{
		"localhost:7070": "vx_token_7070",
		"127.0.0.1:7071": "vx_token_7071",
		"[::1]:8080":     "vx_token_8080",
		"vx.example.com": "vx_token",
		"vx.example:a b": "vx_token",
	} {
		r := httptest.NewRequest("GET", "http://x/ui", nil)
		r.Host = host
		if got := cookieName(r); got != want {
			t.Errorf("cookieName(%q) = %q, want %q", host, got, want)
		}
	}
}
