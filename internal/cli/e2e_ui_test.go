package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// TestUISignInLink runs vx ui --print and follows the link like a browser:
// the page names the account, one POST signs in, the link works once.
func TestUISignInLink(t *testing.T) {
	h, tok := newHarness(t)
	alice := h.user("alice", tok, t.TempDir())
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(alice.must("ui", "--print", "--json")), &out); err != nil || !strings.Contains(out.URL, "/ui/login/code?c=") {
		t.Fatalf("vx ui --json = %+v, %v", out, err)
	}
	if text := alice.must("ui", "--print"); !strings.Contains(text, "/ui/login/code?c=") {
		t.Fatalf("vx ui --print = %s", text)
	}

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	resp, err := browser.Get(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Continue as") || !strings.Contains(string(body), "user:alice") {
		t.Fatalf("code page = %d %s", resp.StatusCode, body)
	}
	u, _ := url.Parse(out.URL)
	form := url.Values{"code": {u.Query().Get("c")}, "next": {"/ui"}}

	// A post from another site is refused.
	req, _ := http.NewRequest(http.MethodPost, h.server+"/ui/login/code", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if r, err := browser.Do(req); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post = %v %v", r.StatusCode, err)
	}

	post := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, h.server+"/ui/login/code", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", h.server)
		r, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := post()
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.Request.URL.Path != "/ui" || !strings.Contains(string(page), "All efforts") {
		t.Fatalf("after sign-in at %s: %s", r.Request.URL, page)
	}
	// The link works once.
	r = post()
	page, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(page), "expired or was already used") {
		t.Fatalf("second use = %s", page)
	}
}
