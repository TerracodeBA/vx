package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store/storetest"
	"github.com/TerracodeBA/vx/internal/version"
)

func publicServer(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	svc := service.New(storetest.New(t), blob.NewMem(), nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, svc
}

// call posts {} to a procedure with the connect JSON protocol and returns
// the HTTP status and the error code in the body, if any.
func call(t *testing.T, srv *httptest.Server, procedure string, header map[string]string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+procedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var e struct{ Code, Message string }
	_ = json.Unmarshal(body, &e)
	return resp.StatusCode, e.Code, e.Message
}

// TestOnlyPublicServiceNeedsNoToken calls every RPC of every service
// without a token: all but PublicService's must answer unauthenticated.
func TestOnlyPublicServiceNeedsNoToken(t *testing.T) {
	srv, _ := publicServer(t)
	services := vxv1.File_vx_v1_vx_proto.Services()
	checked := 0
	for i := range services.Len() {
		sd := services.Get(i)
		for j := range sd.Methods().Len() {
			proc := "/" + string(sd.FullName()) + "/" + string(sd.Methods().Get(j).Name())
			status, code, _ := call(t, srv, proc, nil)
			public := sd.FullName() == vxv1connect.PublicServiceName
			switch {
			case public && code == "unauthenticated":
				t.Errorf("%s needs a token; PublicService must not", proc)
			case public && status == http.StatusNotFound:
				t.Errorf("%s is not mounted", proc)
			case !public && code != "unauthenticated":
				t.Errorf("%s without a token = %d %q, want unauthenticated", proc, status, code)
			}
			checked++
		}
	}
	if checked < 50 {
		t.Fatalf("only %d procedures checked", checked)
	}
}

func TestVersionCheck(t *testing.T) {
	srv, _ := publicServer(t)
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "0.4.0"

	// Too old: refused on any RPC, before the token is even looked at.
	for _, proc := range []string{vxv1connect.AuthServiceWhoAmIProcedure, vxv1connect.PublicServiceStartCLILoginProcedure, vxv1connect.RepoServiceListReposProcedure} {
		_, code, msg := call(t, srv, proc, map[string]string{version.Header: "0.0.1", "Authorization": "Bearer vxt_nope"})
		if code != "failed_precondition" || !strings.Contains(msg, "Update vx: run curl -fsSL "+srv.URL+"/install.sh | sh") {
			t.Errorf("%s from vx 0.0.1 = %q %q", proc, code, msg)
		}
	}
	// Dev builds, no header and current versions pass to authentication.
	for _, v := range []string{"dev", "", "0.4.0", "0.3.0"} {
		if _, code, _ := call(t, srv, vxv1connect.AuthServiceWhoAmIProcedure, map[string]string{version.Header: v}); code != "unauthenticated" {
			t.Errorf("vx %q = %q, want unauthenticated", v, code)
		}
	}
	// ServerInfo answers old CLIs too, and says what to do.
	pub := vxv1connect.NewPublicServiceClient(srv.Client(), srv.URL)
	for v, want := range map[string]string{"0.0.1": "update_required", "0.3.0": "update_recommended", "0.4.0": "ok", "dev": "ok"} {
		req := connect.NewRequest(&vxv1.ServerInfoRequest{CliVersion: v})
		req.Header().Set(version.Header, v)
		resp, err := pub.ServerInfo(context.Background(), req)
		if err != nil {
			t.Fatalf("ServerInfo from %q: %v", v, err)
		}
		m := resp.Msg
		if m.CliStatus != want || m.Version != "0.4.0" || m.MinCliVersion != version.MinCLIVersion || m.GithubSignIn {
			t.Errorf("ServerInfo from %q = %+v", v, m)
		}
		if want != "ok" && !strings.Contains(m.Message, "/install.sh | sh") {
			t.Errorf("message for %q = %q", v, m.Message)
		}
		if !strings.Contains(strings.Join(m.Features, ","), "cli-login") {
			t.Errorf("features = %v", m.Features)
		}
	}
}

func TestStartCLILoginOverConnect(t *testing.T) {
	srv, _ := publicServer(t)
	pub := vxv1connect.NewPublicServiceClient(srv.Client(), srv.URL)
	resp, err := pub.StartCLILogin(context.Background(), connect.NewRequest(&vxv1.StartCLILoginRequest{ClientName: "vx on test"}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.VerificationUrl != srv.URL+"/ui/cli/approve?code="+m.UserCode || m.IntervalSeconds != 2 || m.DeviceCode == "" || m.ExpiresAt == "" {
		t.Fatalf("start = %+v", m)
	}
	poll, err := pub.PollCLILogin(context.Background(), connect.NewRequest(&vxv1.PollCLILoginRequest{DeviceCode: m.DeviceCode}))
	if err != nil || poll.Msg.Status != "pending" || poll.Msg.Token != "" {
		t.Fatalf("poll = %+v %v", poll, err)
	}
	// The rate limit answers resource_exhausted.
	var last error
	for range service.CLILoginStartsPerIP {
		_, last = pub.StartCLILogin(context.Background(), connect.NewRequest(&vxv1.StartCLILoginRequest{}))
	}
	if connect.CodeOf(last) != connect.CodeResourceExhausted {
		t.Fatalf("over the limit = %v", last)
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct{ remote, xff, want string }{
		{"203.0.113.9:1234", "", "203.0.113.9"},
		{"203.0.113.9:1234", "198.51.100.1", "203.0.113.9"},           // a public peer is the client: ignore what it claims
		{"127.0.0.1:1234", "198.51.100.1", "198.51.100.1"},            // local proxy
		{"169.254.1.1:1234", "6.6.6.6, 198.51.100.1", "198.51.100.1"}, // spoofed left entry is ignored
		{"10.0.0.2:1234", "198.51.100.1, 10.0.0.9", "198.51.100.1"},   // internal hops skipped
		{"127.0.0.1:1234", "", "127.0.0.1"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := api.ClientIP(r); got != tc.want {
			t.Errorf("ClientIP(%s, %q) = %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}

// TestServerInfoGitHubSignIn: github_sign_in says whether the server has a
// GitHub App, without a token.
func TestServerInfoGitHubSignIn(t *testing.T) {
	srv, svc := publicServer(t)
	pub := vxv1connect.NewPublicServiceClient(srv.Client(), srv.URL)
	info := func() bool {
		resp, err := pub.ServerInfo(context.Background(), connect.NewRequest(&vxv1.ServerInfoRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GithubSignIn
	}
	if info() {
		t.Fatal("github_sign_in without an App")
	}
	githubtest.New(t).Configure(t, svc)
	if !info() {
		t.Fatal("github_sign_in false with an App")
	}
}
