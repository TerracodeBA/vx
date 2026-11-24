package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/version"
)

// procedures are the API's procedures, the only values the procedure
// label takes; anything else under /vx.v1. is "unknown".
var procedures = func() map[string]bool {
	out := map[string]bool{}
	svcs := vxv1.File_vx_v1_vx_proto.Services()
	for i := 0; i < svcs.Len(); i++ {
		s := svcs.Get(i)
		for j := 0; j < s.Methods().Len(); j++ {
			out["/"+string(s.FullName())+"/"+string(s.Methods().Get(j).Name())] = true
		}
	}
	return out
}()

var webhookHosts = map[string]bool{"github": true, "gitlab": true, "gitea": true, "forgejo": true, "bitbucket": true}

// instrument wraps the server's handler: it records the client address
// for the service, applies the rate limits and counts every request.
// /healthz is neither limited nor counted.
func instrument(limits *ratelimit.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ip := api.ClientIP(r)
		for _, c := range limitChecks(r, ip) {
			if ok, wait := limits.Allow(c.class, c.key); !ok {
				rateLimited(w, r, c, ratelimit.RetrySeconds(wait), ratelimit.RetryText(wait))
				observe(r, http.StatusTooManyRequests, start)
				return
			}
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		r = r.WithContext(service.WithClientIP(r.Context(), ip))
		next.ServeHTTP(sw, r)
		observe(r, sw.status, start)
	})
}

// observe counts one request. r is the request the mux saw, so its
// Pattern names the route.
func observe(r *http.Request, status int, start time.Time) {
	secs := time.Since(start).Seconds()
	st := strconv.Itoa(status)
	p := r.URL.Path
	if strings.HasPrefix(p, "/vx.v1.") {
		proc := p
		if !procedures[proc] {
			proc = "unknown"
		}
		metrics.RPCRequests.Inc(proc, st)
		metrics.RPCDuration.Observe(secs, proc)
		return
	}
	if host, ok := strings.CutPrefix(p, "/hooks/"); ok && r.Method == http.MethodPost {
		if !webhookHosts[host] {
			host = "other"
		}
		metrics.WebhookDeliveries.Inc(host, st)
	}
	route := r.Pattern
	if route == "" {
		route = "unmatched"
	}
	metrics.HTTPRequests.Inc(route, r.Method, st)
	metrics.HTTPDuration.Observe(secs, route)
}

// metricsHandler serves /metrics to callers with the bearer token. Without
// a token configured the endpoint does not exist.
func metricsHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := bearer(r)
		if token == "" {
			http.NotFound(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vx metrics"`)
			http.Error(w, "metrics need the token in VX_METRICS_TOKEN: Authorization: Bearer TOKEN", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		metrics.Default.WriteText(w)
	}
}

// registerGauges points the gauges read at scrape time at this server:
// database pool, queues, version.
func registerGauges(st *store.Store, svc *service.Service) {
	reg := metrics.Default
	reg.SetGaugeFunc("vx_build_info", "The vx-server version; always 1.", func() []metrics.Sample {
		return []metrics.Sample{{Labels: []string{version.String(), runtime.Version()}, Value: 1}}
	}, "version", "go_version")
	reg.SetGaugeFunc("vx_db_pool_connections", "Database pool connections by state: idle, in_use, max.", func() []metrics.Sample {
		s := st.PoolStat()
		return []metrics.Sample{
			{Labels: []string{"idle"}, Value: float64(s.IdleConns())},
			{Labels: []string{"in_use"}, Value: float64(s.AcquiredConns())},
			{Labels: []string{"max"}, Value: float64(s.MaxConns())},
		}
	}, "state")
	reg.SetCounterFunc("vx_db_pool_acquires_total", "Connections taken from the database pool.", func() []metrics.Sample {
		return metrics.One(float64(st.PoolStat().AcquireCount()))
	})
	reg.SetCounterFunc("vx_db_pool_acquire_wait_seconds_total", "Time spent waiting for a free database connection.", func() []metrics.Sample {
		return metrics.One(st.PoolStat().AcquireDuration().Seconds())
	})
	reg.SetCounterFunc("vx_db_pool_empty_acquires_total", "Acquires that had to wait because every connection was in use.", func() []metrics.Sample {
		return metrics.One(float64(st.PoolStat().EmptyAcquireCount()))
	})
	reg.SetGaugeFunc("vx_queue_depth", "Work waiting in background queues: forecast (repositories with overlap forecasts queued), object_backfill (repositories whose objects are not indexed yet).", func() []metrics.Sample {
		out := []metrics.Sample{{Labels: []string{"forecast"}, Value: float64(svc.ForecastQueueDepth())}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if n, err := st.CountObjectBackfills(ctx); err == nil {
			out = append(out, metrics.Sample{Labels: []string{"object_backfill"}, Value: float64(n)})
		}
		return out
	}, "queue")
}

// countedBlobs records every object store operation.
type countedBlobs struct{ blob.Store }

func countBlobs(s blob.Store) blob.Store { return countedBlobs{s} }

func blobResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, blob.ErrNotFound):
		return "not_found"
	}
	return "error"
}

func (c countedBlobs) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	start := time.Now()
	h, err := c.Store.Put(ctx, kind, data)
	metrics.BlobOps.Inc("put", blobResult(err))
	metrics.BlobDuration.Observe(time.Since(start).Seconds(), "put")
	if err == nil {
		metrics.BlobBytes.Add(float64(len(data)), "put")
	}
	return h, err
}

func (c countedBlobs) Get(ctx context.Context, h hash.Hash) (hash.Kind, []byte, error) {
	start := time.Now()
	k, data, err := c.Store.Get(ctx, h)
	metrics.BlobOps.Inc("get", blobResult(err))
	metrics.BlobDuration.Observe(time.Since(start).Seconds(), "get")
	metrics.BlobBytes.Add(float64(len(data)), "get")
	return k, data, err
}

func (c countedBlobs) Has(ctx context.Context, h hash.Hash) (bool, error) {
	start := time.Now()
	ok, err := c.Store.Has(ctx, h)
	metrics.BlobOps.Inc("has", blobResult(err))
	metrics.BlobDuration.Observe(time.Since(start).Seconds(), "has")
	return ok, err
}

// Unwrap returns the store being counted, for code that looks for optional
// methods of a particular store.
func (c countedBlobs) Unwrap() blob.Store { return c.Store }
