package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/cli"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/web"
	"io"
	"log/slog"
)

type harness struct {
	t      *testing.T
	server string
	home   string
	st     *store.Store
}

type user struct {
	h     *harness
	env   map[string]string
	dir   string
	label string
}

func newHarness(t *testing.T) (*harness, string) {
	t.Helper()
	return newHarnessWith(t, false)
}

// newHarnessWith is newHarness; with gitBridge the server imports from and
// ships to git remotes (local paths in tests) through the real git bridge.
func newHarnessWith(t *testing.T, gitBridge bool) (*harness, string) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	blobs, err := blob.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var upstream service.UpstreamSyncer
	if gitBridge {
		upstream = gitbridge.New(st, blobs, t.TempDir(), nil)
	}
	svc := service.New(st, blobs, upstream)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	if err := web.Mount(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(ctx, alice.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, server: srv.URL, home: t.TempDir(), st: st}, tok
}

func (h *harness) user(label, token, dir string) *user {
	return &user{h: h, label: label, dir: dir, env: map[string]string{
		"VX_SERVER": h.server, "VX_TOKEN": token, "VX_CONFIG": filepath.Join(h.home, label+".json"), "VX_MODEL": "claude-opus-5-5",
	}}
}

// run executes vx and returns stdout and the exit code.
func (u *user) run(args ...string) (string, int) {
	u.h.t.Helper()
	var out, errb bytes.Buffer
	app := &cli.App{Out: &out, Err: &errb, Getenv: func(k string) string { return u.env[k] }, Dir: u.dir}
	code := app.Run(context.Background(), args)
	if errb.Len() > 0 {
		out.WriteString(errb.String())
	}
	return out.String(), code
}

// must runs vx and fails the test on a non-zero exit.
func (u *user) must(args ...string) string {
	u.h.t.Helper()
	out, code := u.run(args...)
	if code != 0 {
		u.h.t.Fatalf("[%s] vx %s exited %d:\n%s", u.label, strings.Join(args, " "), code, out)
	}
	return out
}

func (u *user) write(rel, content string) {
	u.h.t.Helper()
	p := filepath.Join(u.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		u.h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		u.h.t.Fatal(err)
	}
}

func (u *user) read(rel string) string {
	u.h.t.Helper()
	data, err := os.ReadFile(filepath.Join(u.dir, rel))
	if err != nil {
		u.h.t.Fatal(err)
	}
	return string(data)
}

func TestTwoAgentsCollaborate(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)

	alice.must("repo", "create", "shop-api")
	alice.must("effort", "new", "Rate limiting", "--repo", "shop-api", "--goal", "Limit login attempts")

	// Alice creates two agents and reads their tokens from JSON output.
	tokenOf := func(name string) string {
		var resp struct{ Token string }
		if err := json.Unmarshal([]byte(alice.must("agent", "add", name, "--json")), &resp); err != nil || resp.Token == "" {
			t.Fatalf("agent add %s: %v", name, err)
		}
		return resp.Token
	}
	a := h.user("backend", tokenOf("backend"), work)
	b := h.user("tests", tokenOf("tests"), work)
	if out := a.must("whoami"); !strings.Contains(out, "agent:alice/backend (acting for user:alice)") {
		t.Fatalf("whoami = %q", out)
	}

	a.must("clone", "shop-api", "rate-limiting", "a")
	b.must("clone", "shop-api", "rate-limiting", "b")
	a.dir = filepath.Join(work, "a")
	b.dir = filepath.Join(work, "b")

	// Agent A lands auth.go.
	a.must("new", "Add login handler")
	a.write("auth.go", "package auth\n\nfunc Login() {\n}\n\nfunc Logout() {\n}\n")
	if out := a.must("status"); !strings.Contains(out, "A auth.go") {
		t.Fatalf("status = %q", out)
	}
	if out := a.must("diff"); !strings.Contains(out, "+func Login() {") {
		t.Fatalf("diff = %q", out)
	}
	out := a.must("land", "--check", "go test ./...=pass:3")
	if !strings.Contains(out, "Landed") {
		t.Fatalf("land = %q", out)
	}

	// Agent B started before that land, adds a different file, and lands.
	// The server merges it onto A's change and B's copy picks up auth.go.
	b.must("new", "Add audit log")
	b.write("audit.go", "package audit\n")
	if out := b.must("status"); !strings.Contains(out, "The effort moved on") {
		t.Fatalf("B should see the effort moved: %q", out)
	}
	out = b.must("land")
	if !strings.Contains(out, "merged your changes onto theirs") {
		t.Fatalf("B land should rebase: %q", out)
	}
	if b.read("auth.go") == "" {
		t.Fatal("B should now have auth.go")
	}

	// Both edit different lines of auth.go: the second land merges line by line.
	a.must("sync")
	a.must("new", "Take a context in Login")
	a.write("auth.go", "package auth\n\nfunc Login(ctx Context) {\n}\n\nfunc Logout() {\n}\n")
	b.must("new", "Audit on logout")
	b.write("auth.go", "package auth\n\nfunc Login() {\n}\n\nfunc Logout() {\n\taudit()\n}\n")
	a.must("land")
	out = b.must("land")
	if !strings.Contains(out, "merged line by line: auth.go") {
		t.Fatalf("expected line merge: %q", out)
	}
	want := "package auth\n\nfunc Login(ctx Context) {\n}\n\nfunc Logout() {\n\taudit()\n}\n"
	if got := b.read("auth.go"); got != want {
		t.Fatalf("B auth.go = %q", got)
	}

	// Both edit the same line: a real conflict.
	a.must("sync")
	a.must("new", "Rename Login to SignIn")
	a.write("auth.go", strings.Replace(want, "func Login(", "func SignIn(", 1))
	b.must("new", "Rename Login to LogIn")
	b.write("auth.go", strings.Replace(want, "func Login(", "func LogIn(", 1))
	a.must("land")
	out, code := b.run("land")
	if code != cli.ExitConflict || !strings.Contains(out, "content") || !strings.Contains(out, "auth.go") {
		t.Fatalf("expected conflict exit, got %d:\n%s", code, out)
	}
	out, code = b.run("sync")
	if code != cli.ExitConflict || !strings.Contains(b.read("auth.go"), "<<<<<<< local") {
		t.Fatalf("sync should mark the conflict, got %d:\n%s\n%s", code, out, b.read("auth.go"))
	}
	if out, code := b.run("land"); code == 0 || !strings.Contains(out, "conflict markers") {
		t.Fatalf("land with markers should be refused: %d %s", code, out)
	}
	b.write("auth.go", strings.Replace(want, "func Login(", "func SignIn(", 1)+"\n// agreed on SignIn\n")
	b.must("land")

	// The log shows every change with its agent and evidence.
	var log struct {
		Changesets []struct {
			Intent   string
			ChangeID string `json:"change_id"`
			Author   struct {
				Principal  string
				OnBehalfOf string `json:"on_behalf_of"`
				Model      string
			}
			Evidence *struct {
				Checks []struct{ Name, Result string }
			}
		}
	}
	if err := json.Unmarshal([]byte(a.must("log", "--json")), &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Changesets) != 6 {
		t.Fatalf("log has %d entries, want 6", len(log.Changesets))
	}
	first := log.Changesets[len(log.Changesets)-1]
	if first.Intent != "Add login handler" || first.Author.Principal != "agent:alice/backend" ||
		first.Author.OnBehalfOf != "user:alice" || first.Author.Model != "claude-opus-5-5" ||
		first.Evidence == nil || first.Evidence.Checks[0].Result != "pass" {
		t.Fatalf("first entry = %+v", first)
	}
	if out := a.must("effort", "show"); !strings.Contains(out, "Rate limiting") {
		t.Fatalf("effort show = %q", out)
	}
	if out := a.must("show", log.Changesets[0].ChangeID); !strings.Contains(out, "agent:alice/tests for user:alice") {
		t.Fatalf("show = %q", out)
	}
}

func TestVerifiedLandAndHonestEvidence(t *testing.T) {
	h, aliceToken := newHarness(t)
	work := t.TempDir()
	alice := h.user("alice", aliceToken, work)
	alice.must("repo", "create", "svc")
	alice.must("effort", "new", "Feature", "--repo", "svc")
	a := h.user("a", aliceToken, work)
	b := h.user("b", aliceToken, work)
	a.must("clone", "svc", "feature", "a")
	b.must("clone", "svc", "feature", "b")
	a.dir, b.dir = filepath.Join(work, "a"), filepath.Join(work, "b")

	// A lands first.
	a.must("new", "Add one")
	a.write("one.txt", "1\n")
	a.must("land")

	// B's plain land is merged by the server: the evidence is marked.
	b.must("new", "Add two")
	b.write("two.txt", "2\n")
	out := b.must("land", "--check", "unit=pass:3")
	if !strings.Contains(out, "evidence was gathered before that merge") {
		t.Fatalf("land output should flag the merge: %s", out)
	}
	show := b.must("show", "--repo", "svc", strings.Fields(out)[1])
	if !strings.Contains(show, "merged onto newer work") || !strings.Contains(show, "check unit: pass (reported, 3 passed, 0 failed)") {
		t.Fatalf("show = %s", show)
	}

	// --require-up-to-date refuses to merge.
	a.must("new", "Add three")
	a.write("three.txt", "3\n")
	b.must("sync")
	b.must("new", "Add four")
	b.write("four.txt", "4\n")
	a.must("land")
	if out, code := b.run("land", "--require-up-to-date"); code != cli.ExitConflict || !strings.Contains(out, "moved on") {
		t.Fatalf("require-up-to-date: %d %s", code, out)
	}

	// --run syncs first, runs the check on the merged files, lands unmerged.
	out = b.must("land", "--run", "test -f three.txt && test -f four.txt")
	if !strings.Contains(out, "syncing before running checks") || !strings.Contains(out, "pass test -f three.txt") || strings.Contains(out, "merged your changes") {
		t.Fatalf("verified land = %s", out)
	}
	logOut := b.must("log", "-n", "1")
	if !strings.Contains(logOut, "run by vx") || strings.Contains(logOut, "merged onto newer work") {
		t.Fatalf("log = %s", logOut)
	}
	// A failing --run check stops the land.
	b.must("new", "Broken")
	b.write("five.txt", "5\n")
	if out, code := b.run("land", "--run", "exit 3"); code == 0 || !strings.Contains(out, "checks failed") {
		t.Fatalf("failing check should stop land: %d %s", code, out)
	}

	// Change-id prefixes resolve.
	var lj struct {
		Changesets []struct {
			ChangeID string `json:"change_id"`
		}
	}
	if err := json.Unmarshal([]byte(a.must("log", "--json")), &lj); err != nil {
		t.Fatal(err)
	}
	if out := a.must("show", lj.Changesets[0].ChangeID[:4]); !strings.Contains(out, lj.Changesets[0].ChangeID) {
		t.Fatalf("prefix show = %s", out)
	}

	// vx wait returns once someone lands.
	a.must("sync")
	done := make(chan string, 1)
	go func() {
		out, _ := a.run("wait", "--timeout", "20s", "--interval", "50ms")
		done <- out
	}()
	b.must("land", "-m", "Add five", "--fresh") // after the failing check above
	if out := <-done; !strings.Contains(out, "Add five") {
		t.Fatalf("wait = %s", out)
	}
	if out := a.must("status"); !strings.Contains(out, "Recently landed") {
		t.Fatalf("status = %s", out)
	}
}

func TestAgentEnvFile(t *testing.T) {
	h, tok := newHarness(t)
	dir := t.TempDir()
	alice := h.user("alice", tok, dir)
	env := filepath.Join(dir, "agents", "bot.env")
	out := alice.must("agent", "add", "bot", "--env-file", env)
	if strings.Contains(out, "vxt_") {
		t.Fatal("token must not be printed with --env-file")
	}
	info, err := os.Stat(env)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode = %v, %v", info.Mode(), err)
	}
	data, _ := os.ReadFile(env)
	if !strings.Contains(string(data), "export VX_TOKEN=\"vxt_") || !strings.Contains(string(data), h.server) {
		t.Fatalf("env file = %s", data)
	}
	if _, code := alice.run("agent", "add", "bot2", "--env-file", env); code == 0 {
		t.Fatal("existing env file must not be overwritten")
	}
}
