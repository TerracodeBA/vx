package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/version"
)

// PublicService answers without a token: server info and the browser
// login of the CLI. Everything else goes through authInterceptor.

type requestInfoKey struct{}

// requestInfo is what handlers need from the HTTP request that connect
// does not pass on: the server's own base URL as the client reached it,
// and the client's address.
type requestInfo struct {
	base string
	ip   string
}

func infoFrom(ctx context.Context) requestInfo {
	ri, _ := ctx.Value(requestInfoKey{}).(requestInfo)
	return ri
}

// withRequestInfo records the base URL and client address of r.
func withRequestInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		ri := requestInfo{base: scheme + "://" + r.Host, ip: ClientIP(r)}
		ctx := context.WithValue(r.Context(), requestInfoKey{}, ri)
		next.ServeHTTP(w, r.WithContext(service.WithClientIP(ctx, ri.ip)))
	})
}

// ClientIP is the address a request came from. Behind a proxy on a
// private or loopback address (Cloud Run, a local reverse proxy), it is the
// last public address in X-Forwarded-For: the one the proxy saw, not what
// the client claims further left.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !(peer.IsLoopback() || peer.IsPrivate() || peer.IsLinkLocalUnicast()) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	return host
}

// baseURL is the URL clients reach this server at: PublicURL when set,
// else what the request says.
func (h *Handlers) baseURL(ctx context.Context) string {
	if u := h.svc.GitHub.PublicURL; u != "" {
		return strings.TrimRight(u, "/")
	}
	return infoFrom(ctx).base
}

// versionInterceptor refuses CLIs older than version.MinCLIVersion on
// every call but ServerInfo, which says so itself. Clients that send no
// version (curl, the MCP endpoint, older CLIs from before the header) and
// dev builds pass; each dev version is logged once.
func versionInterceptor(svc *service.Service, log *slog.Logger) connect.UnaryInterceptorFunc {
	var seen sync.Map
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			v := strings.TrimSpace(req.Header().Get(version.Header))
			if v == "" || req.Spec().Procedure == vxv1connect.PublicServiceServerInfoProcedure {
				return next(ctx, req)
			}
			if version.IsDev(v) {
				if _, dup := seen.LoadOrStore(v, true); !dup {
					log.Info("allowing a dev build of the vx CLI", "cli_version", v, "min_cli_version", version.MinCLIVersion)
				}
				return next(ctx, req)
			}
			base := infoFrom(ctx).base
			if status, msg := svc.CheckCLIVersion(v, base); status == service.CLIUpdateRequired {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
			}
			return next(ctx, req)
		}
	}
}

func (h *Handlers) ServerInfo(ctx context.Context, req *connect.Request[vxv1.ServerInfoRequest]) (*connect.Response[vxv1.ServerInfoResponse], error) {
	info := h.svc.Info(ctx)
	cliVersion := req.Msg.CliVersion
	if cliVersion == "" {
		cliVersion = req.Header().Get(version.Header)
	}
	status, msg := h.svc.CheckCLIVersion(cliVersion, h.baseURL(ctx))
	return connect.NewResponse(&vxv1.ServerInfoResponse{
		Version: info.Version, MinCliVersion: info.MinCLIVersion, Features: info.Features,
		GithubSignIn: info.GitHubSignIn, CliStatus: status, Message: msg,
	}), nil
}

func (h *Handlers) StartCLILogin(ctx context.Context, req *connect.Request[vxv1.StartCLILoginRequest]) (*connect.Response[vxv1.StartCLILoginResponse], error) {
	l, err := h.svc.StartCLILogin(ctx, req.Msg.ClientName)
	if err != nil {
		return nil, toConnect(err)
	}
	link := l.VerificationURL
	if strings.HasPrefix(link, "/") {
		link = h.baseURL(ctx) + link
	}
	return connect.NewResponse(&vxv1.StartCLILoginResponse{
		DeviceCode: l.DeviceCode, UserCode: l.UserCode, VerificationUrl: link,
		ExpiresAt: ts(l.ExpiresAt), IntervalSeconds: int32(l.Interval.Seconds()),
	}), nil
}

func (h *Handlers) PollCLILogin(ctx context.Context, req *connect.Request[vxv1.PollCLILoginRequest]) (*connect.Response[vxv1.PollCLILoginResponse], error) {
	status, token, p, err := h.svc.PollCLILogin(ctx, req.Msg.DeviceCode)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.PollCLILoginResponse{Status: status, Token: token}
	if token != "" {
		a, err := h.svc.ResolveActor(ctx, p)
		if err != nil {
			return nil, toConnect(err)
		}
		out.Principal = h.principal(ctx, a)
	}
	return connect.NewResponse(out), nil
}
