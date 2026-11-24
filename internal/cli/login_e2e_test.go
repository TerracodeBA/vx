package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/cli"
	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/version"
	"github.com/TerracodeBA/vx/internal/web"
)

// browser is a person's browser: a cookie jar that does not follow
// redirects, so tests see where pages send it.
type browser struct {
	t      *testing.T
	server string
	c      *http.Client
}

func newBrowser(t *testing.T, server string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, server: server, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) get(link string) (int, string, string) {
	b.t.Helper()
	resp, err := b.c.Get(link)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

func (b *browser) post(path string, form url.Values, origin string) (int, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.server+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// signIn signs the browser in with a token, the way the login form does.
func (b *browser) signIn(token string) {
	b.t.Helper()
	if status, _ := b.post("/ui/login", url.Values{"token": {token}, "next": {"/ui"}}, b.server); status != http.StatusSeeOther {
		b.t.Fatalf("sign in = %d", status)
	}
}

// loginApp is a vx with its own config file and no VX_TOKEN.
func loginApp(dir string, out, errw io.Writer, open func(string) error) *cli.App {
	env := map[string]string{"VX_CONFIG": filepath.Join(dir, "config.json"), "HOME": dir}
	return &cli.App{Out: out, Err: errw, Getenv: func(k string) string { return env[k] }, Dir: dir, OpenBrowser: open}
}

// TestLoginThroughBrowser runs vx login without a token: the "browser" is
// sent through sign-in and back, sees the code, approves; vx saves a token
// that works, and the login cannot be replayed.
func TestLoginThroughBrowser(t *testing.T) {
	h, aliceToken := newHarness(t)
	home := t.TempDir()

	opened := ""
	open := func(link string) error {
		opened = link
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		code := u.Query().Get("code")
		b := newBrowser(t, h.server)
		// Not signed in: sign in first, then come back to this page.
		status, loc, _ := b.get(link)
		if status != http.StatusFound || loc != "/ui/login?next="+url.QueryEscape(u.RequestURI()) {
			t.Errorf("signed-out approval page = %d %q", status, loc)
		}
		b.signIn(aliceToken)
		status, _, body := b.get(link)
		if status != http.StatusOK || !strings.Contains(body, "Authorize vx on testbox for <span class=\"mono\">user:alice</span>?") ||
			!strings.Contains(body, `<p class="user-code" aria-label="Login code">`+code+`</p>`) {
			t.Errorf("approval page = %d %s", status, body)
		}
		// A post from another site is refused; nothing is approved.
		if status, _ := b.post("/ui/cli/approve", url.Values{"code": {code}, "decision": {"approve"}}, "https://evil.example"); status != http.StatusForbidden {
			t.Errorf("cross-site approve = %d", status)
		}
		status, body = b.post("/ui/cli/approve", url.Values{"code": {code}, "decision": {"approve"}}, h.server)
		if status != http.StatusOK || !strings.Contains(body, "vx on testbox is authorized") {
			t.Errorf("approve = %d %s", status, body)
		}
		return nil
	}
	var out, errb bytes.Buffer
	code := loginApp(home, &out, &errb, open).Run(context.Background(), []string{"login", "--server", h.server + "/", "--name", "vx on testbox"})
	if code != 0 {
		t.Fatalf("vx login exited %d: %s %s", code, out.String(), errb.String())
	}
	if !strings.HasPrefix(opened, h.server+"/ui/cli/approve?code=") {
		t.Fatalf("opened %q", opened)
	}
	text := out.String()
	for _, want := range []string{"Your login code: ", "Opened it in your browser.", "Logged in to " + h.server + " as user:alice\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Server, Token string
		ServerInfo    *struct{ Features []string } `json:"server_info"`
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Server != h.server || !strings.HasPrefix(saved.Token, "vxt_") || saved.Token == aliceToken || saved.ServerInfo == nil {
		t.Fatalf("config = %s (%v)", data, err)
	}
	if fi, _ := os.Stat(filepath.Join(home, "config.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode())
	}
	// The saved token works.
	out.Reset()
	if code := loginApp(home, &out, &errb, nil).Run(context.Background(), []string{"whoami"}); code != 0 || out.String() != "user:alice\n" {
		t.Fatalf("whoami = %d %q %s", code, out.String(), errb.String())
	}
	alice := h.user("alice", aliceToken, home)
	if list := alice.must("token", "list"); !strings.Contains(list, "vx on testbox (browser login)") {
		t.Fatalf("token list lacks the browser login token:\n%s", list)
	}
}

// TestLoginDeniedNoBrowser: --no-browser only prints the link; the person
// denies, and vx says so and saves nothing.
func TestLoginDeniedNoBrowser(t *testing.T) {
	h, aliceToken := newHarness(t)
	home := t.TempDir()
	pr, pw := io.Pipe()
	var errb bytes.Buffer
	done := make(chan int, 1)
	app := loginApp(home, pw, &errb, func(string) error { t.Error("--no-browser opened a browser"); return nil })
	go func() {
		done <- app.Run(context.Background(), []string{"login", "--server", h.server, "--no-browser"})
		pw.Close()
	}()
	var text strings.Builder
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		text.WriteString(line + "\n")
		if code, ok := strings.CutPrefix(line, "Your login code: "); ok {
			b := newBrowser(t, h.server)
			b.signIn(aliceToken)
			if status, body := b.post("/ui/cli/approve", url.Values{"code": {strings.ToLower(code)}, "decision": {"deny"}}, h.server); status != http.StatusOK || !strings.Contains(body, "Denied") {
				t.Errorf("deny = %d %s", status, body)
			}
		}
	}
	if code := <-done; code != 1 || !strings.Contains(errb.String(), "the login was denied in the browser") {
		t.Fatalf("vx login = %d, %s\n%s", code, errb.String(), text.String())
	}
	if strings.Contains(text.String(), "Opened it") {
		t.Fatalf("output: %s", text.String())
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a denied login saved the config: %v", err)
	}
}

// TestLoginCancelled: Ctrl-C while waiting says so and saves nothing.
func TestLoginCancelled(t *testing.T) {
	h, _ := newHarness(t)
	home := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	app := loginApp(home, &out, &errb, func(string) error { cancel(); return nil })
	if code := app.Run(ctx, []string{"login", "--server", h.server}); code != 1 || !strings.Contains(errb.String(), "login cancelled; nothing was saved") {
		t.Fatalf("cancelled login = %d %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled login saved the config")
	}
}

// TestLoginWithToken: --token and VX_TOKEN keep working.
func TestLoginWithToken(t *testing.T) {
	h, tok := newHarness(t)
	home := t.TempDir()
	var out, errb bytes.Buffer
	if code := loginApp(home, &out, &errb, nil).Run(context.Background(), []string{"login", "--server", h.server, "--token", tok}); code != 0 || !strings.Contains(out.String(), "Logged in to "+h.server+" as user:alice") {
		t.Fatalf("login --token = %d %s %s", code, out.String(), errb.String())
	}
	u := h.user("env", tok, home)
	delete(u.env, "VX_SERVER")
	if text := u.must("login", "--server", h.server); !strings.Contains(text, "as user:alice") || !strings.Contains(text, "VX_TOKEN") {
		t.Fatalf("login with VX_TOKEN = %s", text)
	}
	if text, code := u.run("login", "--server", h.server, "--token", "vxt_wrong"); code != 1 || !strings.Contains(text, "does not accept this token") {
		t.Fatalf("bad token = %d %s", code, text)
	}
}

// TestOldCLIRefused: a release vx older than the server's minimum gets
// "update vx" with the install line on every command.
func TestOldCLIRefused(t *testing.T) {
	h, tok := newHarness(t)
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "0.0.1"
	u := h.user("old", tok, t.TempDir())
	out, code := u.run("whoami")
	if code != 1 || !strings.Contains(out, "Update vx: run curl -fsSL "+h.server+"/install.sh | sh") {
		t.Fatalf("old vx whoami = %d %s", code, out)
	}
	if out, code := u.run("login", "--server", h.server); code != 1 || !strings.Contains(out, "too old for this server") {
		t.Fatalf("old vx login = %d %s", code, out)
	}
}

// fakeOldServer is a vx-server that answers ServerInfo and WhoAmI only.
type fakeOldServer struct {
	vxv1connect.UnimplementedPublicServiceHandler
	vxv1connect.UnimplementedAuthServiceHandler
	infoCalls atomic.Int32
}

func (f *fakeOldServer) ServerInfo(context.Context, *connect.Request[vxv1.ServerInfoRequest]) (*connect.Response[vxv1.ServerInfoResponse], error) {
	f.infoCalls.Add(1)
	return connect.NewResponse(&vxv1.ServerInfoResponse{
		Version: "9.9.9", MinCliVersion: "0.1.0", Features: []string{"board"}, CliStatus: "update_recommended",
		Message: "vx-server runs 9.9.9, this vx is 0.2.0. Update vx: run curl -fsSL https://vx.example.com/install.sh | sh",
	}), nil
}

func (f *fakeOldServer) WhoAmI(context.Context, *connect.Request[vxv1.WhoAmIRequest]) (*connect.Response[vxv1.WhoAmIResponse], error) {
	return connect.NewResponse(&vxv1.WhoAmIResponse{Principal: &vxv1.Principal{Ref: "user:alice"}}), nil
}

// TestServerNotices: a newer server recommends updating once a day, and a
// server without a feature says so in one line instead of "404".
func TestServerNotices(t *testing.T) {
	f := &fakeOldServer{}
	mux := http.NewServeMux()
	mux.Handle(vxv1connect.NewPublicServiceHandler(f))
	mux.Handle(vxv1connect.NewAuthServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	home := t.TempDir()
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := loginApp(home, &out, &errb, nil).Run(context.Background(), args)
		return code, out.String(), errb.String()
	}
	code, _, errText := run("login", "--server", srv.URL, "--token", "vxt_fake")
	if code != 0 || !strings.Contains(errText, "vx: notice: vx-server runs 9.9.9") || !strings.Contains(errText, "install.sh | sh") {
		t.Fatalf("login = %d %q", code, errText)
	}
	// The answer is remembered: later commands do not ask again today.
	before := f.infoCalls.Load()
	if code, out, errText := run("whoami"); code != 0 || out != "user:alice\n" || errText != "" {
		t.Fatalf("whoami = %d %q %q", code, out, errText)
	}
	if f.infoCalls.Load() != before {
		t.Fatal("ServerInfo was asked again within a day")
	}
	code, _, errText = run("tasks", "--effort", "some-effort", "--repo", "acme/api")
	if code != 1 || !strings.Contains(errText, "this vx server ("+srv.URL+", vx-server 9.9.9) has no tasks; ask whoever runs it to upgrade vx-server") {
		t.Fatalf("tasks on a server without tasks = %d %q", code, errText)
	}
}

// TestLoginOldServerWithoutBrowserLogin: a server from before
// PublicService tells people to use a token.
func TestLoginOldServerWithoutBrowserLogin(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	code := loginApp(t.TempDir(), &out, &errb, nil).Run(context.Background(), []string{"login", "--server", srv.URL})
	if code != 1 || !strings.Contains(errb.String(), "runs an older vx-server without browser login; log in with a token instead") {
		t.Fatalf("login = %d %q", code, errb.String())
	}
}

// TestLoginWithGitHubOneFlow is the whole path for someone new: vx login
// opens the approval page, the browser is sent to sign in, signs in with
// GitHub (which creates the account), comes back to the same page, and
// approves; vx is then logged in as that person.
func TestLoginWithGitHubOneFlow(t *testing.T) {
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc.GitHub.PublicURL, svc.GitHub.Signup = srv.URL, service.SignupOpen
	gh := githubtest.New(t)
	gh.Configure(t, svc)
	gh.AddUser("nina")

	open := func(link string) error {
		b := newBrowser(t, srv.URL)
		follow := &http.Client{Jar: b.c.Jar}
		_, loc, _ := b.get(link)
		if !strings.HasPrefix(loc, "/ui/login?next=") {
			t.Fatalf("signed-out approval page sent the browser to %q", loc)
		}
		next, _ := url.ParseQuery(strings.TrimPrefix(loc, "/ui/login?"))
		gh.SignIn("nina")
		resp, err := follow.Get(srv.URL + "/ui/login/github?next=" + url.QueryEscape(next.Get("next")))
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(page), "url="+html.EscapeString(next.Get("next"))) {
			t.Fatalf("after GitHub the browser does not go back to the approval page:\n%s", page)
		}
		status, _, body := b.get(srv.URL + next.Get("next"))
		if status != http.StatusOK || !strings.Contains(body, "for <span class=\"mono\">user:nina</span>?") {
			t.Fatalf("approval page after GitHub sign-in = %d\n%s", status, body)
		}
		code := strings.TrimPrefix(next.Get("next"), "/ui/cli/approve?code=")
		if status, body := b.post("/ui/cli/approve", url.Values{"code": {code}, "decision": {"approve"}}, srv.URL); status != http.StatusOK || !strings.Contains(body, "is authorized") {
			t.Fatalf("approve = %d %s", status, body)
		}
		return nil
	}
	home := t.TempDir()
	var out, errb bytes.Buffer
	if code := loginApp(home, &out, &errb, open).Run(context.Background(), []string{"login", "--server", srv.URL}); code != 0 {
		t.Fatalf("vx login = %d %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Logged in to "+srv.URL+" as user:nina") {
		t.Fatalf("output:\n%s", out.String())
	}
	out.Reset()
	if code := loginApp(home, &out, &errb, nil).Run(context.Background(), []string{"whoami"}); code != 0 || out.String() != "user:nina\n" {
		t.Fatalf("whoami = %d %q %s", code, out.String(), errb.String())
	}
}
