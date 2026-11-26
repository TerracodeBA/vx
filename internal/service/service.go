// Package service is vx's application logic: creating repos and efforts,
// opening workspaces and landing changes. It is independent of the API
// transport; internal/api adapts it to connect-go.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/store"
)

// Code classifies service errors so the API layer can map them.
type Code int

const (
	CodeInvalid Code = iota + 1
	CodeNotFound
	CodeExists
	CodePermission
	CodeFailedPrecondition
	CodeUnavailable
	CodeTooMany // rate limited: try again later
)

// Error is a service error with a code and a message meant for the caller.
type Error struct {
	Code Code
	Msg  string
	Err  error
}

// PermissionDenied starts the message of every permission error, so
// people, agents and scripts can recognise one whatever it goes on to say.
const PermissionDenied = "permission denied: "

func (e *Error) Error() string {
	msg := e.Msg
	if e.Err != nil && e.Msg == "" {
		msg = e.Err.Error()
	}
	if e.Code == CodePermission {
		return WithPermissionDenied(msg)
	}
	return msg
}

// WithPermissionDenied puts PermissionDenied in front of msg unless it is
// already there.
func WithPermissionDenied(msg string) string {
	if strings.HasPrefix(msg, PermissionDenied) {
		return msg
	}
	return PermissionDenied + msg
}

func (e *Error) Unwrap() error { return e.Err }

func errf(code Code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Classify turns store and blob errors into service errors and passes
// others on unchanged.
func Classify(err error) error { return classify(err) }

// classify turns store errors into service errors and passes others on.
func classify(err error) error {
	var se *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return err
	case errors.Is(err, store.ErrNotFound), errors.Is(err, blob.ErrNotFound):
		return &Error{Code: CodeNotFound, Msg: err.Error(), Err: err}
	case errors.Is(err, store.ErrExists):
		return &Error{Code: CodeExists, Msg: err.Error(), Err: err}
	}
	return err
}

// UpstreamSyncer imports branches from a repo's git provider.
type UpstreamSyncer interface {
	// DefaultBranch asks the provider for the repo's default branch.
	DefaultBranch(ctx context.Context, repo store.Repo) (string, error)
	// SyncBranch imports new commits on branch and records its head.
	SyncBranch(ctx context.Context, repo store.Repo, branch string) (store.Branch, int, error)
}

// Service holds the dependencies of the application logic.
type Service struct {
	Store    *store.Store
	Blobs    blob.Store
	Upstream UpstreamSyncer // nil: repos without an upstream only
	Shipper  Shipper        // nil: shipping disabled
	// Hosts configures GitLab, Gitea, Forgejo and Bitbucket webhooks.
	Hosts HostSettings
	Now   func() time.Time
	// MaxLandAttempts bounds compare-and-swap retries when several agents
	// land into the same effort at once.
	MaxLandAttempts int

	// GitHub configures sign-in with GitHub and GitHub-backed repositories;
	// its API is nil until a GitHub App is set up.
	GitHub GitHubSettings

	// ForecastTimeout bounds one background overlap forecast run; zero:
	// DefaultForecastTimeout.
	ForecastTimeout time.Duration

	// Limits are the rate limits (sign-up here; the server applies the
	// others per request); nil: none. Quotas are the organization quotas
	// (quotas.go). Audit configures the operation log export (audit.go).
	Limits *ratelimit.Limiter
	Quotas QuotaSettings
	Audit  AuditSettings

	// Push configures Web Push notifications (push.go).
	Push PushSettings

	// Agreement configures the data processing agreement step (dpa.go);
	// off by default.
	Agreement AgreementSettings

	// OrgDeletion configures deleting and purging organizations
	// (org_delete.go, org_purge.go).
	OrgDeletion OrgDeletionSettings
	// purgeHook, for tests, runs before every batch of a purge.
	purgeHook func(step string)

	gh        githubState // the GitHub App loaded from the database
	forecasts forecaster  // background overlap forecasts
	objects   objectIndex // repositories whose objects need no backfill
	sizes     objectSizer // records how many bytes new objects take
}

// New returns a Service with defaults filled in.
// When upstream can also ship (the git bridge can), it is used for that too.
func New(st *store.Store, blobs blob.Store, upstream UpstreamSyncer) *Service {
	s := &Service{Store: st, Blobs: blobs, Upstream: upstream, Now: time.Now, MaxLandAttempts: 25}
	if sh, ok := upstream.(Shipper); ok {
		s.Shipper = sh
	}
	return s
}

// Actor is the authenticated caller, with the human it acts for.
type Actor struct {
	store.Principal
	Parent *store.Principal // set for agents
	// TokenID is the token the request was made with; uuid.Nil when the
	// caller did not authenticate with a token (tests, internal calls).
	TokenID uuid.UUID
}

// ResolveActor loads the parent of an agent principal.
func (s *Service) ResolveActor(ctx context.Context, p store.Principal) (Actor, error) {
	a := Actor{Principal: p}
	if p.ParentID != nil {
		parent, err := s.Store.PrincipalByID(ctx, *p.ParentID)
		if err != nil {
			return a, err
		}
		a.Parent = &parent
	}
	return a, nil
}

// Human returns the human behind the actor: the actor itself or its parent.
func (a Actor) Human() store.Principal {
	if a.Parent != nil {
		return *a.Parent
	}
	return a.Principal
}

// OnBehalfOf is the parent's ref for agents, empty otherwise.
func (a Actor) OnBehalfOf() string {
	if a.Parent != nil {
		return a.Parent.Ref()
	}
	return ""
}
