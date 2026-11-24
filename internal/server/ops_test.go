package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

func opsServer(t *testing.T, cfg Config) (*Server, *httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	cfg.DatabaseURL = storetest.NewDatabase(t)
	cfg.DataDir = t.TempDir()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	_, tok, _, err := srv.Service.AddPerson(ctx, nil, service.NewPerson{Handle: "alice", CreateOrg: true})
	if err != nil {
		t.Fatal(err)
	}
	return srv, ts, tok
}

func get(t *testing.T, url, token string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// TestMetricsEndpoint: /metrics is off without VX_METRICS_TOKEN, needs the
// token when on, and shows request, land, pool, queue and version metrics
// without repository or person names.
func TestMetricsEndpoint(t *testing.T) {
	_, off, _ := opsServer(t, Config{})
	if code, _, _ := get(t, off.URL+"/metrics", ""); code != http.StatusNotFound {
		t.Fatalf("/metrics without a token configured = %d, want 404", code)
	}

	srv, ts, tok := opsServer(t, Config{MetricsToken: "scrape-secret"})
	if code, body, _ := get(t, ts.URL+"/metrics", "wrong"); code != http.StatusUnauthorized || !strings.Contains(body, "VX_METRICS_TOKEN") {
		t.Fatalf("/metrics with a wrong token = %d %s", code, body)
	}
	ctx := context.Background()
	c := client.New(ts.URL, tok)
	if _, err := c.Repos.CreateRepo(ctx, connect.NewRequest(&vxv1.CreateRepoRequest{Name: "alice/secret-project"})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Efforts.CreateEffort(ctx, connect.NewRequest(&vxv1.CreateEffortRequest{Repo: "alice/secret-project", Name: "Hidden effort"})); err != nil {
		t.Fatal(err)
	}
	get(t, ts.URL+"/ui/login", "")
	get(t, ts.URL+"/vx.v1.Nope/Nothing", "")
	code, body, h := get(t, ts.URL+"/metrics", "scrape-secret")
	if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("/metrics = %d %s", code, h.Get("Content-Type"))
	}
	for _, want := range []string{
		`vx_rpc_requests_total{procedure="/vx.v1.RepoService/CreateRepo",status="200"}`,
		`vx_rpc_duration_seconds_bucket{procedure="/vx.v1.EffortService/CreateEffort",le="+Inf"}`,
		`vx_rpc_requests_total{procedure="unknown",status="404"}`,
		`vx_http_requests_total{route="GET /ui/login",method="GET",status="200"}`,
		`vx_operations_total{op="effort.create"}`,
		`vx_db_pool_connections{state="max"}`,
		`vx_queue_depth{queue="forecast"}`,
		`vx_queue_depth{queue="object_backfill"}`,
		`vx_build_info{version="`,
		`vx_blob_ops_total{op="put",result="ok"}`,
		"# TYPE vx_lands_total counter",
		"# TYPE vx_rate_limited_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	for _, leak := range []string{"secret-project", "Hidden", "alice"} {
		if strings.Contains(body, leak) {
			t.Errorf("/metrics names %q", leak)
		}
	}
	_ = srv
}

// TestRateLimits: API calls per token answer ResourceExhausted, sign-in
// and webhooks 429, all with "slow down: ... retry in N s" and
// Retry-After.
func TestRateLimits(t *testing.T) {
	_, ts, tok := opsServer(t, Config{Limits: ratelimit.Config{
		ratelimit.APIToken: {PerSecond: 0.01, Burst: 2},
		ratelimit.SignIn:   {PerSecond: 0.01, Burst: 1},
		ratelimit.Webhook:  {PerSecond: 0.01, Burst: 1},
	}})
	ctx := context.Background()
	c := client.New(ts.URL, tok)
	for i := 0; i < 2; i++ {
		if _, err := c.Auth.WhoAmI(ctx, connect.NewRequest(&vxv1.WhoAmIRequest{})); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	_, err := c.Auth.WhoAmI(ctx, connect.NewRequest(&vxv1.WhoAmIRequest{}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted ||
		!strings.Contains(ce.Message(), "slow down: too many requests with this token; retry in ") {
		t.Fatalf("3rd call = %v", err)
	}
	if ce.Meta().Get("Retry-After") == "" {
		t.Error("no Retry-After on the API refusal")
	}
	// Another token is not limited by the first one's bucket.
	if _, err := client.New(ts.URL, "not-a-token").Auth.WhoAmI(ctx, connect.NewRequest(&vxv1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("other token = %v", err)
	}

	if code, _, _ := get(t, ts.URL+"/ui/login", ""); code != http.StatusOK {
		t.Fatalf("first sign-in page = %d", code)
	}
	code, body, h := get(t, ts.URL+"/ui/login", "")
	if code != http.StatusTooManyRequests || !strings.Contains(body, "slow down: too many sign-in attempts from your address; retry in") || h.Get("Retry-After") == "" {
		t.Fatalf("second sign-in page = %d %q retry-after=%q", code, body, h.Get("Retry-After"))
	}
	// Other pages are not limited.
	if code, _, _ := get(t, ts.URL+"/healthz", ""); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}

	post := func() int {
		resp, err := http.Post(ts.URL+"/hooks/gitlab", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(); code == http.StatusTooManyRequests {
		t.Fatal("first webhook limited")
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("second webhook = %d, want 429", code)
	}
}

func TestLimitClasses(t *testing.T) {
	for path, want := range map[string]string{
		"/vx.v1.WorkspaceService/Land":       "api_token,api_ip",
		"/vx.v1.PublicService/StartCLILogin": "api_token,api_ip,cli_login",
		"/mcp":                               "api_token,api_ip",
		"/ui/login/github":                   "signin",
		"/ui/login/sso":                      "signin",
		"/auth/oidc/callback":                "signin",
		"/hooks/github":                      "webhook",
		"/ui/repos":                          "",
		"/ui/loginx":                         "",
		"/vx.v1.PublicService/ServerInfo":    "api_token,api_ip",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer t")
		var got []string
		for _, c := range limitChecks(r, "203.0.113.1") {
			got = append(got, string(c.class))
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: classes %v, want %s", path, got, want)
		}
	}
	r := httptest.NewRequest(http.MethodPost, service.CLIApprovePath, nil)
	if c := limitChecks(r, "x"); len(c) != 1 || c[0].class != ratelimit.CLILogin {
		t.Errorf("CLI approval: %v", c)
	}
}
