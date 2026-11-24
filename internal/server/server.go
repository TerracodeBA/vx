// Package server wires the vx server together: database, blob store, git
// bridge, API handlers and the HTTP listener.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/github"
	"github.com/TerracodeBA/vx/internal/mcp"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/web"
)

// Config is the server configuration.
type Config struct {
	Addr        string // listen address, e.g. ":7070"
	DatabaseURL string
	DataDir     string // blob store and git mirrors live here
	Logger      *slog.Logger
	// Upstream imports branches from git providers; nil disables it.
	Upstream func(st *store.Store, blobs blob.Store, dataDir string) (service.UpstreamSyncer, error)
	// GitHub configures the GitHub App (VX_PUBLIC_URL, VX_SECRET_KEY,
	// VX_SIGNUP). Connect defaults to the internal/github client.
	GitHub service.GitHubSettings
	// Hosts configures GitLab, Gitea, Forgejo and Bitbucket webhooks
	// (VX_<HOST>_WEBHOOK_SECRET).
	Hosts service.HostSettings
	// MetricsToken turns on /metrics for callers with this bearer token
	// (VX_METRICS_TOKEN); empty: no /metrics.
	MetricsToken string
	// Limits are the rate limits (VX_RATE_*); nil: none.
	Limits ratelimit.Config
	// Quotas are the organization quotas' defaults (VX_QUOTA_*); the data
	// directory's disk is watched either way.
	Quotas service.QuotaSettings
	// Audit configures the operation log (VX_AUDIT_RETENTION).
	Audit service.AuditSettings
}

// Server is a running vx server.
type Server struct {
	Store   *store.Store
	Blobs   blob.Store
	Service *service.Service
	Handler http.Handler
	log     *slog.Logger
}

// New opens the database (applying migrations) and builds the handler.
func New(ctx context.Context, cfg Config) (*Server, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	disk, err := blob.NewDisk(filepath.Join(cfg.DataDir, "objects"))
	if err != nil {
		st.Close()
		return nil, err
	}
	blobs := countBlobs(disk) // vx_blob_* metrics
	var up service.UpstreamSyncer
	if cfg.Upstream != nil {
		if up, err = cfg.Upstream(st, blobs, cfg.DataDir); err != nil {
			st.Close()
			return nil, err
		}
	}
	svc := service.New(st, blobs, up)
	svc.GitHub = cfg.GitHub
	svc.Hosts = cfg.Hosts
	if cfg.Limits != nil {
		svc.Limits = ratelimit.New(cfg.Limits)
	}
	svc.Quotas, svc.Audit = cfg.Quotas, cfg.Audit
	if svc.Quotas.DataDir == "" {
		svc.Quotas.DataDir = cfg.DataDir
	}
	if svc.GitHub.Connect == nil {
		apiURL, webURL := svc.GitHub.APIURL, svc.GitHub.WebURL
		svc.GitHub.Connect = func(creds service.GitHubAppCredentials) (service.GitHubAPI, error) {
			return github.FromCredentials(creds, apiURL, webURL)
		}
	}
	// A GitHub App set up earlier must open with this server's key.
	if err := svc.LoadGitHubApp(ctx); err != nil {
		st.Close()
		return nil, err
	}
	// Upgrade notices (superadmins, #33) are logged once.
	if err := svc.LogUpgradeNotices(ctx, log); err != nil {
		log.Warn("could not read upgrade notices", "err", err)
	}
	// Repositories of an installation fetch, push and open pull requests
	// with its installation token.
	if b, ok := up.(interface {
		UseRepoTokens(func(context.Context, store.Repo) (string, error))
	}); ok {
		b.UseRepoTokens(svc.GitHubRepoToken)
	}
	// GitLab, Gitea, Forgejo and Bitbucket repositories with a token of
	// their own use it.
	if b, ok := up.(interface {
		UseHostTokens(func(context.Context, store.Repo) (string, error))
	}); ok {
		b.UseHostTokens(svc.HostRepoToken)
	}
	// Repositories from before objects were scoped to repositories get their
	// objects indexed: queue them now, so object requests for them wait for
	// their backfill, and walk them in the background once serving.
	if n, err := st.QueueObjectBackfills(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("queue object backfill: %w", err)
	} else if n > 0 {
		log.Info("queued repositories for object backfill", "count", n)
	}
	mux := http.NewServeMux()
	api.MountWithLog(mux, svc, log)
	mcp.Mount(mux, svc, log)
	if err := web.Mount(mux, svc, log); err != nil {
		st.Close()
		return nil, err
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /metrics", metricsHandler(cfg.MetricsToken))
	registerGauges(st, svc)
	return &Server{Store: st, Blobs: blobs, Service: svc, Handler: logRequests(log, instrument(svc.Limits, mux)), log: log}, nil
}

// Close releases the database.
func (s *Server) Close() { s.Store.Close() }

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully.
// It logs why it stopped: context.Cause(ctx), which names the signal when
// ctx comes from signal.NotifyContext.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(s.Handler, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	if s.Service != nil {
		go s.backfillObjects(ctx)
		go s.Service.RunPushNotifier(ctx, s.log)
		go s.Service.PruneAuditLoop(ctx, s.log)
	}
	s.log.Info("vx server listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.log.Info("vx server shutting down", "reason", context.Cause(ctx).Error(), "grace", shutdownGrace)
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		s.log.Error("vx server did not shut down cleanly", "err", err)
		return err
	}
	if s.Service != nil {
		// Let overlap forecasts finish within the grace period; one cut
		// short is recomputed when someone next asks for it.
		done := make(chan struct{})
		go func() { s.Service.WaitForecasts(); close(done) }()
		select {
		case <-done:
		case <-shutdown.Done():
			s.log.Warn("overlap forecasts still running at shutdown; they are recomputed on demand")
		}
	}
	s.log.Info("vx server stopped")
	return nil
}

// backfillObjects indexes the objects of repositories queued for a
// backfill, one repository at a time. Requests for a queued repository
// backfill it on demand, so they do not wait for the others.
func (s *Server) backfillObjects(ctx context.Context) {
	done, err := s.Service.BackfillObjects(ctx, s.log)
	if err != nil && ctx.Err() == nil {
		s.log.Error("object backfill stopped; it resumes on the next start or on first use of each repository", "err", err)
		return
	}
	if len(done) > 0 {
		s.log.Info("object backfill finished", "repos", len(done))
	}
}

// shutdownGrace is how long in-flight requests get to finish on shutdown.
const shutdownGrace = 20 * time.Second

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}
