package web_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// The For you page lists the escalated question, the review alice may
// approve and the overlap on her effort, newest first; answering and
// approving work from there and refuse cross-origin posts and agents;
// someone who cannot read the repository sees none of it.
func TestForYouPage(t *testing.T) {
	e := newPhoneEnv(t)
	code, body := e.get("/ui/for-you")
	if code != 200 {
		t.Fatalf("for you = %d\n%s", code, body)
	}
	for _, want := range []string{
		`id="n-` + e.quest + `"`, "Escalated question", `id="r-` + e.review + `"`, "Users table", "Overlap", "Shared helpers",
		`action="/ui/for-you/answer"`, `action="/ui/for-you/review"`, `value="approve"`, `value="reject"`,
		"Both have trade-offs &lt;b&gt;here&lt;/b&gt;", "1</span> question", "1</span> review",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("for you lacks %q", want)
		}
	}
	// Newest first: the question escalated (a minute after it was asked)
	// after the review and the overlap were posted.
	if q, r := strings.Index(body, `id="n-`+e.quest), strings.Index(body, `id="r-`+e.review); q > r {
		t.Errorf("escalated question after the older review")
	}

	// Carol cannot read acme/shop: nothing of it shows.
	carol, _ := e.st.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "carol"})
	carolTok, _ := e.st.CreateToken(e.ctx, carol.ID, "web")
	cw := newWebEnvShared(e.webEnv)
	cw.signIn(carolTok)
	resp, _ := cw.c.Get(e.srv.URL + "/ui/for-you")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "Nothing waits for you") || strings.Contains(string(b), "acme/shop") || strings.Contains(string(b), e.quest) {
		t.Fatalf("carol sees acme/shop on For you:\n%s", b)
	}

	// An agent's session shows its page but no forms, and cannot post.
	_, agTok, err := e.svc.CreateAgent(e.ctx, e.alice, "phone-test", "")
	if err != nil {
		t.Fatal(err)
	}
	aw := newWebEnvShared(e.webEnv)
	aw.signIn(agTok)
	resp, _ = aw.c.Get(e.srv.URL + "/ui/for-you")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(b), `action="/ui/for-you/answer"`) {
		t.Fatalf("agent's For you = %d, has answer form: %v", resp.StatusCode, strings.Contains(string(b), "for-you/answer"))
	}
	same := map[string]string{"Origin": e.srv.URL}
	if code, body := aw.post("/ui/for-you/answer", url.Values{"note": {e.quest}, "body": {"x"}}, same); code != http.StatusForbidden || !strings.Contains(body, "only people answer") {
		t.Fatalf("agent answering = %d", code)
	}
	if code, _ := aw.post("/ui/for-you/review", url.Values{"review": {e.review}, "decision": {"approve"}}, same); code != http.StatusForbidden {
		t.Fatalf("agent approving = %d", code)
	}

	// Cross-origin posts are refused and change nothing.
	for _, h := range []map[string]string{{}, {"Origin": "http://evil.example"}, {"Referer": "http://evil.example/ui/for-you"}} {
		if code, _ := e.post("/ui/for-you/answer", url.Values{"note": {e.quest}, "body": {"evil"}}, h); code != http.StatusForbidden {
			t.Errorf("answer with %v = %d", h, code)
		}
		if code, _ := e.post("/ui/for-you/review", url.Values{"review": {e.review}, "decision": {"approve"}}, h); code != http.StatusForbidden {
			t.Errorf("approve with %v = %d", h, code)
		}
	}
	if n, _ := e.svc.GetNote(e.ctx, e.alice, e.quest); n.State != service.NoteEscalated {
		t.Fatalf("question after refused posts: %s", n.State)
	}

	// Answer and approve from the page.
	nofollow := &http.Client{Jar: e.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, form url.Values) string {
		t.Helper()
		resp, err := postForm(nofollow, e.srv.URL, path, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	if loc := post("/ui/for-you/answer", url.Values{"note": {e.quest}, "body": {"The account id."}}); loc != "/ui/for-you?done=answered" {
		t.Fatalf("answer went to %s", loc)
	}
	if n, _ := e.svc.GetNote(e.ctx, e.alice, e.quest); n.State != service.NoteAnswered {
		t.Fatalf("question = %s", n.State)
	}
	if loc := post("/ui/for-you/review", url.Values{"review": {e.review}, "decision": {"approve"}}); loc != "/ui/for-you?done=approved" {
		t.Fatalf("approve went to %s", loc)
	}
	_, body = e.get("/ui/for-you?done=approved")
	if strings.Contains(body, `id="n-`+e.quest) || strings.Contains(body, `id="r-`+e.review) || !strings.Contains(body, "Approved.") {
		t.Fatalf("answered and approved items still listed:\n%s", body)
	}
}

// The manifest, the service worker, the offline page and the icons are
// served with the types browsers require.
func TestPhoneAppAssets(t *testing.T) {
	e := newWebEnv(t)
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		return resp, b
	}
	resp, b := get("/ui/manifest.webmanifest")
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("manifest type = %q", ct)
	}
	var m struct {
		Name, StartURL, Scope, Display, ThemeColor string
		Icons                                      []struct{ Src, Sizes, Type, Purpose string }
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if raw["start_url"] != "/ui/for-you" || raw["scope"] != "/ui/" || raw["display"] != "standalone" || raw["theme_color"] == nil || len(m.Icons) < 3 {
		t.Fatalf("manifest = %s", b)
	}
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		r, _ := get(ic.Src)
		if r.Header.Get("Content-Type") != "image/png" {
			t.Errorf("%s type = %q", ic.Src, r.Header.Get("Content-Type"))
		}
		sizes[ic.Sizes+" "+ic.Purpose] = true
	}
	if !sizes["192x192 any"] || !sizes["512x512 any"] || !sizes["512x512 maskable"] {
		t.Errorf("icons = %v", sizes)
	}
	get("/ui/static/apple-touch-icon.png")
	resp, b = get("/ui/sw.js")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("sw.js type = %q, cache = %q", ct, resp.Header.Get("Cache-Control"))
	}
	for _, want := range []string{`"push"`, `"notificationclick"`, `"/ui/offline"`, "showNotification"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("sw.js lacks %s", want)
		}
	}
	if resp, _ := get("/ui/static/phone.js"); !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Errorf("phone.js type = %q", resp.Header.Get("Content-Type"))
	}
	resp, b = get("/ui/offline")
	if !strings.Contains(string(b), "You are offline") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Errorf("offline page: %s", b)
	}
}

// fakePushService records what the server pushes.
type fakePushService struct {
	*httptest.Server
	mu     sync.Mutex
	got    []http.Header
	status int
}

func newFakePushService(t *testing.T) *fakePushService {
	f := &fakePushService{status: http.StatusCreated}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, r.Header.Clone())
		st := f.status
		f.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePushService) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

// Turning notifications on stores the browser's subscription, needs a
// same-origin post from a person, and makes the server push questions
// escalated to them and new reviews once each; a gone subscription is
// dropped.
func TestPushSubscriptions(t *testing.T) {
	e := newPhoneEnv(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e.svc.GitHub.SecretKey = key
	push := newFakePushService(t)
	e.svc.Push = service.PushSettings{Client: push.Client(), AllowAnyEndpoint: true}

	// The server's key: made once, kept sealed.
	_, kb := e.get("/ui/push/key")
	var k struct{ Key string }
	if err := json.Unmarshal([]byte(kb), &k); err != nil || len(k.Key) < 80 {
		t.Fatalf("key = %s", kb)
	}
	if _, kb2 := e.get("/ui/push/key"); !strings.Contains(kb2, k.Key) {
		t.Fatal("the key changed")
	}
	row, err := e.st.PushKey(e.ctx)
	if err != nil || row.PublicKey != k.Key || len(row.PrivateKeyEnc) < 32+16 {
		t.Fatalf("stored key = %+v %v", row, err)
	}

	// The browser's side of a subscription.
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	b64 := base64.RawURLEncoding.EncodeToString
	sub := url.Values{"endpoint": {push.URL + "/send/alice-phone"}, "p256dh": {b64(ua.PublicKey().Bytes())}, "auth": {b64([]byte("0123456789abcdef"))}, "device": {"Chrome on Android"}}
	js := map[string]string{"Origin": e.srv.URL, "Accept": "application/json"}
	for _, h := range []map[string]string{{}, {"Origin": "http://evil.example"}} {
		if code, _ := e.post("/ui/push/subscribe", sub, h); code != http.StatusForbidden {
			t.Errorf("subscribe with %v = %d", h, code)
		}
	}
	e.svc.Push.AllowAnyEndpoint = false // a real server: only public https push services
	if code, body := e.post("/ui/push/subscribe", sub, js); code != http.StatusBadRequest || !strings.Contains(body, "https") {
		t.Fatalf("http endpoint = %d %s", code, body)
	}
	e.svc.Push.AllowAnyEndpoint = true
	_, agTok, _ := e.svc.CreateAgent(e.ctx, e.alice, "phone-test", "")
	aw := newWebEnvShared(e.webEnv)
	aw.signIn(agTok)
	if code, _ := aw.post("/ui/push/subscribe", sub, js); code != http.StatusForbidden {
		t.Fatalf("agent subscribing = %d", code)
	}
	if subs, _ := e.st.PushSubscriptions(e.ctx, e.alice.ID); len(subs) != 0 {
		t.Fatalf("refused posts stored %d subscriptions", len(subs))
	}

	// Turned on now, with the server clock back to the real time: the
	// question escalates a minute after it was asked, after this.
	e.shift = 0
	if code, body := e.post("/ui/push/subscribe", sub, js); code != 200 {
		t.Fatalf("subscribe = %d %s", code, body)
	}
	if code, _ := e.post("/ui/push/subscribe", sub, js); code != 200 { // again: the same row
		t.Fatal("subscribing again failed")
	}
	subs, _ := e.st.PushSubscriptions(e.ctx, e.alice.ID)
	if len(subs) != 1 || subs[0].Device != "Chrome on Android" {
		t.Fatalf("subscriptions = %+v", subs)
	}
	if _, body := e.get("/ui/settings"); !strings.Contains(body, `id="notifications"`) || !strings.Contains(body, "Chrome on Android") || !strings.Contains(body, `data-push-on`) {
		t.Fatalf("settings lacks the device:\n%s", body)
	}
	// A new review after turning notifications on.
	res := e.landFiles(e.aliceAg, e.effort, "Orders table", map[string]string{"db/003_orders.sql": "create table orders ();\n"}, nil)
	if res.Review == nil {
		t.Fatal("no review")
	}
	n, err := e.svc.NotifyPush(e.ctx)
	if err != nil || n != 1 || push.count() != 1 { // the question has not escalated yet
		t.Fatalf("first run sent %d (%d at the push service): %v", n, push.count(), err)
	}
	e.shift = 2 * time.Minute
	if n, err := e.svc.NotifyPush(e.ctx); err != nil || n != 1 || push.count() != 2 {
		t.Fatalf("after the deadline sent %d (%d): %v", n, push.count(), err)
	}
	h := push.got[1]
	if h.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(h.Get("Authorization"), "vapid t=") || !strings.HasSuffix(h.Get("Authorization"), "k="+k.Key) {
		t.Fatalf("push headers = %v", h)
	}
	if n, _ := e.svc.NotifyPush(e.ctx); n != 0 || push.count() != 2 {
		t.Fatalf("pushed again: %d", n)
	}
	subs, _ = e.st.PushSubscriptions(e.ctx, e.alice.ID)
	if subs[0].LastSentAt == nil || subs[0].LastError != "" {
		t.Fatalf("subscription after sending = %+v", subs[0])
	}

	// The test button; then the push service forgets the browser.
	if code, body := e.post("/ui/push/test", url.Values{"endpoint": sub["endpoint"]}, js); code != 200 || push.count() != 3 {
		t.Fatalf("test = %d %s", code, body)
	}
	push.mu.Lock()
	push.status = http.StatusGone
	push.mu.Unlock()
	if code, _ := e.post("/ui/push/test", url.Values{"endpoint": sub["endpoint"]}, js); code == 200 {
		t.Fatal("test to a gone subscription succeeded")
	}
	if subs, _ := e.st.PushSubscriptions(e.ctx, e.alice.ID); len(subs) != 0 {
		t.Fatalf("gone subscription kept: %+v", subs)
	}

	// Turning off with the plain form (no script).
	push.status = http.StatusCreated
	e.post("/ui/push/subscribe", sub, js)
	if code, _ := e.post("/ui/push/unsubscribe", url.Values{"endpoint": sub["endpoint"]}, map[string]string{"Origin": e.srv.URL}); code != 200 {
		t.Fatalf("unsubscribe = %d", code)
	}
	if subs, _ := e.st.PushSubscriptions(e.ctx, e.alice.ID); len(subs) != 0 {
		t.Fatalf("still subscribed: %+v", subs)
	}
}
