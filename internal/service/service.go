// Package service is vx's application logic: creating repos and efforts,
// opening workspaces and landing changes. It is independent of the API
// transport; internal/api adapts it to connect-go.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TerracodeBA/vx/internal/blob"
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
)

// Error is a service error with a code and a message meant for the caller.
type Error struct {
	Code Code
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil && e.Msg == "" {
		return e.Err.Error()
	}
	return e.Msg
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
	Now      func() time.Time
	// MaxLandAttempts bounds compare-and-swap retries when several agents
	// land into the same effort at once.
	MaxLandAttempts int
}

// New returns a Service with defaults filled in.
func New(st *store.Store, blobs blob.Store, upstream UpstreamSyncer) *Service {
	return &Service{Store: st, Blobs: blobs, Upstream: upstream, Now: time.Now, MaxLandAttempts: 25}
}

// Actor is the authenticated caller, with the human it acts for.
type Actor struct {
	store.Principal
	Parent *store.Principal // set for agents
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
