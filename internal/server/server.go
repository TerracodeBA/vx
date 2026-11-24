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
	blobs, err := blob.NewDisk(filepath.Join(cfg.DataDir, "objects"))
	if err != nil {
		st.Close()
		return nil, err
	}
	var up service.UpstreamSyncer
	if cfg.Upstream != nil {
		if up, err = cfg.Upstream(st, blobs, cfg.DataDir); err != nil {
			st.Close()
			return nil, err
		}
	}
	svc := service.New(st, blobs, up)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
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
	return &Server{Store: st, Blobs: blobs, Service: svc, Handler: logRequests(log, mux), log: log}, nil
}

// Close releases the database.
func (s *Server) Close() { s.Store.Close() }

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(s.Handler, &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info("vx server listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

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
