package service

// Browser login for the CLI, device-code style (RFC 8628 in spirit):
//
//  1. `vx login --server URL` calls StartCLILogin (no token). The server
//     returns a secret device code, which only the CLI knows, and a short
//     user code the person compares in the browser.
//  2. The person opens /ui/cli/approve?code=USER-CODE, signs in (with
//     GitHub or a token) and approves or denies.
//  3. The CLI polls PollCLILogin with the device code. After approval the
//     first poll creates the token and returns it; later polls say "used".
//
// Only sha256 of the device code is stored. The token is created when the
// CLI collects it, in the transaction that marks the login used, so no
// token rests in the database, not even encrypted, and it is handed out
// exactly once.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/version"
)

// CLILogin is a started browser login.
type CLILogin struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	Interval        time.Duration
}

// CLILogin states. Used means the CLI collected its token; slow down
// asks the CLI to poll less often (the login stays pending).
const (
	CLILoginPending  = "pending"
	CLILoginApproved = "approved"
	CLILoginDenied   = "denied"
	CLILoginExpired  = "expired"
	CLILoginUsed     = "used"
	CLILoginSlowDown = "slow_down"
)

// Limits of browser logins.
const (
	CLILoginTTL      = 10 * time.Minute
	CLILoginInterval = 2 * time.Second
	// CLILoginStartsPerIP logins may be started from one address per
	// CLILoginRateWindow.
	CLILoginStartsPerIP = 10
	CLILoginRateWindow  = 10 * time.Minute
	// MaxPendingCLILogins bounds undecided logins server-wide, so nobody
	// fills the table from many addresses.
	MaxPendingCLILogins = 2000
	// MaxCLIClientName is the longest client name kept.
	MaxCLIClientName = 60
)

// CLIApprovePath is the approval page; the user code goes in ?code=.
const CLIApprovePath = "/ui/cli/approve"

// userCodeAlphabet is consonants only: no vowels (no words), no digits and
// so no 0/O or 1/I lookalikes. 8 of 20 letters: 20^8 = 2.6e10 codes.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

type clientIPKey struct{}

// WithClientIP records the caller's network address for rate limits.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

func clientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

func hashDeviceCode(code string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return sum[:]
}

// NormalizeUserCode upper-cases a user code and puts the dash back, so
// "wdjb mjht" and "WDJBMJHT" find WDJB-MJHT. It returns "" for anything
// that cannot be a user code.
func NormalizeUserCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch {
		case r == '-' || unicode.IsSpace(r):
			continue
		case strings.ContainsRune(userCodeAlphabet, r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	s := b.String()
	if len(s) != 8 {
		return ""
	}
	return s[:4] + "-" + s[4:]
}

func newUserCode() (string, error) {
	out := make([]byte, 0, 9)
	var buf [16]byte
	for len(out) < 9 {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		for _, b := range buf {
			// 240 = 12*20: rejecting larger bytes keeps letters uniform.
			if b >= 240 || len(out) == 9 {
				continue
			}
			if len(out) == 4 {
				out = append(out, '-')
			}
			out = append(out, userCodeAlphabet[int(b)%len(userCodeAlphabet)])
		}
	}
	return string(out), nil
}

func cleanClientName(name string) string {
	name = strings.Join(strings.FieldsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if name == "" {
		return "vx CLI"
	}
	if r := []rune(name); len(r) > MaxCLIClientName {
		name = string(r[:MaxCLIClientName])
	}
	return name
}

// StartCLILogin begins a browser login for a CLI (no token needed).
func (s *Service) StartCLILogin(ctx context.Context, clientName string) (CLILogin, error) {
	now := s.Now()
	ip := clientIP(ctx)
	// Old rows only matter for the rate limit window; drop them as we go.
	if err := s.Store.DeleteCLILoginsBefore(ctx, now.Add(-CLILoginRateWindow-time.Hour)); err != nil {
		return CLILogin{}, err
	}
	if ip != "" {
		n, err := s.Store.CountCLILoginsSince(ctx, ip, now.Add(-CLILoginRateWindow))
		if err != nil {
			return CLILogin{}, err
		}
		if n >= CLILoginStartsPerIP {
			return CLILogin{}, errf(CodeTooMany, "too many logins started from your address (%d in %s); wait a few minutes and run vx login again, or log in with a token: vx login --server URL --token TOKEN", n, CLILoginRateWindow)
		}
	}
	if n, err := s.Store.CountPendingCLILogins(ctx, now); err != nil {
		return CLILogin{}, err
	} else if n >= MaxPendingCLILogins {
		return CLILogin{}, errf(CodeTooMany, "this server has too many logins waiting for approval; try again in a few minutes")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return CLILogin{}, err
	}
	device := base64.RawURLEncoding.EncodeToString(raw[:])
	out := CLILogin{DeviceCode: device, ExpiresAt: now.Add(CLILoginTTL), Interval: CLILoginInterval}
	for attempt := 0; ; attempt++ {
		code, err := newUserCode()
		if err != nil {
			return CLILogin{}, err
		}
		err = s.Store.CreateCLILogin(ctx, store.CLILogin{
			DeviceHash: hashDeviceCode(device), UserCode: code, ClientName: cleanClientName(clientName),
			ClientIP: ip, ExpiresAt: out.ExpiresAt,
		})
		if errors.Is(err, store.ErrExists) && attempt < 5 {
			continue
		}
		if err != nil {
			return CLILogin{}, err
		}
		out.UserCode = code
		break
	}
	out.VerificationURL = strings.TrimRight(s.GitHub.PublicURL, "/") + CLIApprovePath + "?code=" + url.QueryEscape(out.UserCode)
	return out, nil
}

// PollCLILogin returns the state of a login; on approval it returns the new
// token exactly once. Later polls return CLILoginUsed. Polling faster than
// the interval returns CLILoginSlowDown while the login is pending.
func (s *Service) PollCLILogin(ctx context.Context, deviceCode string) (status, token string, p store.Principal, err error) {
	if strings.TrimSpace(deviceCode) == "" {
		return "", "", store.Principal{}, errf(CodeInvalid, "device code is empty")
	}
	h := hashDeviceCode(deviceCode)
	now := s.Now()
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		l, err := tx.CLILoginByDevice(ctx, h)
		if errors.Is(err, store.ErrNotFound) {
			return errf(CodeNotFound, "this login is unknown or long finished; run vx login again")
		}
		if err != nil {
			return err
		}
		expired := !now.Before(l.ExpiresAt)
		switch {
		case l.Status == CLILoginUsed:
			status = CLILoginUsed
			return nil
		case l.Status == CLILoginDenied:
			status = CLILoginDenied
			return nil
		case expired:
			status = CLILoginExpired
			return nil
		case l.Status == CLILoginPending:
			tooFast := l.LastPolledAt != nil && now.Sub(*l.LastPolledAt) < CLILoginInterval*3/4
			if err := tx.TouchCLILoginPoll(ctx, h, now); err != nil {
				return err
			}
			status = CLILoginPending
			if tooFast {
				status = CLILoginSlowDown
			}
			return nil
		case l.Status != CLILoginApproved || l.PrincipalID == nil:
			return errf(CodeFailedPrecondition, "login is in an unexpected state %q; run vx login again", l.Status)
		}
		ok, err := tx.SetCLILoginStatus(ctx, h, CLILoginApproved, CLILoginUsed, nil, now)
		if err != nil {
			return err
		}
		if !ok { // collected by a concurrent poll
			status = CLILoginUsed
			return nil
		}
		if p, err = tx.PrincipalByID(ctx, *l.PrincipalID); err != nil {
			return err
		}
		name := l.ClientName + " (browser login)"
		if r := []rune(name); len(r) > MaxTokenName {
			name = string(r[:MaxTokenName])
		}
		tok, secret, err := tx.IssueToken(ctx, p.ID, name)
		if err != nil {
			return err
		}
		token, status = secret, CLILoginApproved
		return tx.LogOp(ctx, nil, &p.ID, "token.create", p.Ref(), nil,
			map[string]string{"token": tok.ID.String(), "name": tok.Name, "via": "browser login"})
	})
	if err != nil {
		return "", "", store.Principal{}, classify(err)
	}
	return status, token, p, nil
}

// pendingCLILogin finds a login the actor may decide, locked when tx is a
// transaction.
func (s *Service) pendingCLILogin(ctx context.Context, q interface {
	CLILoginByUserCode(context.Context, string) (store.CLILogin, error)
}, actor Actor, userCode string) (store.CLILogin, error) {
	if actor.Kind != store.KindHuman {
		return store.CLILogin{}, errf(CodePermission, "only people can authorize a vx CLI; %s is an agent. Agents get their token from `vx agent add`", actor.Ref())
	}
	code := NormalizeUserCode(userCode)
	gone := errf(CodeNotFound, "no login is waiting for code %q: it expired (codes last %s), was already used, or is mistyped. Run vx login again for a new code", strings.TrimSpace(userCode), CLILoginTTL)
	if code == "" {
		return store.CLILogin{}, gone
	}
	l, err := q.CLILoginByUserCode(ctx, code)
	if errors.Is(err, store.ErrNotFound) {
		return store.CLILogin{}, gone
	}
	if err != nil {
		return store.CLILogin{}, err
	}
	if l.Status != CLILoginPending || !s.Now().Before(l.ExpiresAt) {
		return store.CLILogin{}, gone
	}
	return l, nil
}

// PendingCLILogin returns the client name of a pending login by user code,
// for the approval page (signed-in people only).
func (s *Service) PendingCLILogin(ctx context.Context, actor Actor, userCode string) (clientName string, expires time.Time, err error) {
	l, err := s.pendingCLILogin(ctx, s.Store, actor, userCode)
	if err != nil {
		return "", time.Time{}, classify(err)
	}
	return l.ClientName, l.ExpiresAt, nil
}

// ApproveCLILogin approves or denies a pending login by user code; approval
// lets the CLI collect a token for the person named after the client.
func (s *Service) ApproveCLILogin(ctx context.Context, actor Actor, userCode string, approve bool) error {
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		l, err := s.pendingCLILogin(ctx, tx, actor, userCode)
		if err != nil {
			return err
		}
		to, op := CLILoginDenied, "cli_login.deny"
		if approve {
			to, op = CLILoginApproved, "cli_login.approve"
		}
		ok, err := tx.SetCLILoginStatus(ctx, l.DeviceHash, CLILoginPending, to, &actor.ID, s.Now())
		if err != nil {
			return err
		}
		if !ok {
			return errf(CodeNotFound, "this login was decided a moment ago; run vx login again if needed")
		}
		return tx.LogOp(ctx, nil, &actor.ID, op, actor.Ref(), nil,
			map[string]string{"client": l.ClientName, "user_code": l.UserCode, "ip": l.ClientIP})
	})
	return classify(err)
}

// ServerInfo is the server's version and the oldest CLI it accepts.
type ServerInfo struct {
	Version       string
	MinCLIVersion string
	Features      []string
	GitHubSignIn  bool
}

// Features this server has, for CLIs that need to tell "your server is
// too old" apart from other errors.
var Features = []string{
	"board", "claims", "tasks", "rebase", "orgs", "github", "cli-login", "install-script",
	// v0.3: overlap forecasts, vx.toml rules, reservations, closing efforts,
	// agent scopes and budgets, landing policies, merge by declaration
	"overlaps", "rules", "reserve", "close", "agent-scopes", "policies", "merge-by-declaration",
	// v0.4: review comments and CI results reach agents (Inbox.notes,
	// Shipment.ci_*), automatic rebases, the vx evidence check run
	"reviews", "ci", "auto-rebase", "evidence-check",
}

// CLI version states in ServerInfo answers.
const (
	CLIOK                = "ok"
	CLIUpdateRecommended = "update_recommended"
	CLIUpdateRequired    = "update_required"
)

// Info describes this server.
func (s *Service) Info(ctx context.Context) ServerInfo {
	return ServerInfo{
		Version:       version.Version,
		MinCLIVersion: version.MinCLIVersion,
		Features:      append([]string(nil), Features...),
		GitHubSignIn:  s.GitHubSignInEnabled(ctx),
	}
}

// InstallCommand is the one-liner that installs vx from the server at
// baseURL (PublicURL wins when set).
func (s *Service) InstallCommand(baseURL string) string {
	if s.GitHub.PublicURL != "" {
		baseURL = s.GitHub.PublicURL
	}
	return "curl -fsSL " + strings.TrimRight(baseURL, "/") + "/install.sh | sh"
}

// CheckCLIVersion says whether a CLI of cliVersion works with this server,
// and what to tell its user. Dev builds and CLIs that send no version are
// always ok.
func (s *Service) CheckCLIVersion(cliVersion, baseURL string) (status, message string) {
	if version.IsDev(cliVersion) {
		return CLIOK, ""
	}
	install := s.InstallCommand(baseURL)
	if c, _ := version.Compare(cliVersion, version.MinCLIVersion); c < 0 {
		return CLIUpdateRequired, "this vx (" + cliVersion + ") is too old for this server, which needs vx " + version.MinCLIVersion + " or newer. Update vx: run " + install
	}
	if c, ok := version.Compare(cliVersion, version.Version); ok && c < 0 {
		return CLIUpdateRecommended, "vx-server runs " + version.Version + ", this vx is " + cliVersion + ". Update vx: run " + install
	}
	return CLIOK, ""
}
