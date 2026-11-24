package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/mcp"
	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/service"
)

// limitCheck is one bucket a request takes from.
type limitCheck struct {
	class ratelimit.Class
	key   string
	what  string // for the refusal: "too many requests with this token"
}

// limitChecks says which buckets r takes from, by path:
//
//   - the API (/vx.v1.*) and MCP: per token, and per client address;
//     starting a CLI login also takes from the CLI login rate
//   - /ui/login, /ui/login/* and /auth/*: sign-in, per address (GitHub and
//     single sign-on start pages and callbacks)
//   - the CLI login approval page: CLI login, per address
//   - /hooks/*: webhook deliveries, per address
//
// Sign-up cannot be told from sign-in by its path; the service limits it
// where it creates the account (Service.AllowSignup). Other pages of the
// web UI are not limited.
func limitChecks(r *http.Request, ip string) []limitCheck {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/vx.v1.") || p == mcp.Path || strings.HasPrefix(p, mcp.Path+"/"):
		var out []limitCheck
		if tok := bearer(r); tok != "" {
			out = append(out, limitCheck{ratelimit.APIToken, tokenKey(tok), "too many requests with this token"})
		}
		out = append(out, limitCheck{ratelimit.APIIP, ip, "too many requests from your address"})
		if p == vxv1connect.PublicServiceStartCLILoginProcedure {
			out = append(out, limitCheck{ratelimit.CLILogin, ip, "too many logins started from your address"})
		}
		return out
	case p == "/ui/login" || strings.HasPrefix(p, "/ui/login/") || strings.HasPrefix(p, "/auth/"):
		return []limitCheck{{ratelimit.SignIn, ip, "too many sign-in attempts from your address"}}
	case p == service.CLIApprovePath && r.Method == http.MethodPost:
		return []limitCheck{{ratelimit.CLILogin, ip, "too many logins approved from your address"}}
	case strings.HasPrefix(p, "/hooks/"):
		return []limitCheck{{ratelimit.Webhook, ip, "too many webhook deliveries from your address"}}
	}
	return nil
}

func bearer(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

// tokenKey keys a token's bucket by a hash, so the limiter never holds
// tokens. Invalid tokens get buckets too, and also take from their
// address's bucket.
func tokenKey(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:12])
}

// rateLimited answers a refused request: ResourceExhausted in the
// protocol of an API call, 429 for everything else, both with Retry-After
// and "slow down: WHAT; retry in N s".
func rateLimited(w http.ResponseWriter, r *http.Request, c limitCheck, wait int, retry string) {
	metrics.RateLimited.Inc(string(c.class))
	msg := "slow down: " + c.what + "; " + retry
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	if strings.HasPrefix(r.URL.Path, "/vx.v1.") {
		_ = connect.NewErrorWriter().Write(w, r, connect.NewError(connect.CodeResourceExhausted, errors.New(msg)))
		return
	}
	http.Error(w, msg, http.StatusTooManyRequests)
}
