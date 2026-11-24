// Package api adapts the service layer to connect-go handlers.
package api

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

type actorKey struct{}

// actorFrom returns the authenticated caller. The interceptor guarantees it
// is set for every handler.
func actorFrom(ctx context.Context) service.Actor {
	a, _ := ctx.Value(actorKey{}).(service.Actor)
	return a
}

// authInterceptor resolves the bearer token on every request.
func authInterceptor(svc *service.Service) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			header := req.Header().Get("Authorization")
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || token == "" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token: run `vx login` or set VX_TOKEN"))
			}
			p, tokenID, err := svc.Store.AuthenticateToken(ctx, strings.TrimSpace(token))
			if errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("token not valid or revoked"))
			}
			if err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, err)
			}
			actor, err := svc.ResolveActor(ctx, p)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			actor.TokenID = tokenID
			return next(context.WithValue(ctx, actorKey{}, actor), req)
		}
	}
}

// toConnect maps service errors to connect codes.
func toConnect(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	var se *service.Error
	if errors.As(err, &se) {
		code := connect.CodeInternal
		switch se.Code {
		case service.CodeInvalid:
			code = connect.CodeInvalidArgument
		case service.CodeNotFound:
			code = connect.CodeNotFound
		case service.CodeExists:
			code = connect.CodeAlreadyExists
		case service.CodePermission:
			code = connect.CodePermissionDenied
		case service.CodeFailedPrecondition:
			code = connect.CodeFailedPrecondition
		case service.CodeUnavailable:
			code = connect.CodeUnavailable
		case service.CodeTooMany:
			code = connect.CodeResourceExhausted
		}
		return connect.NewError(code, errors.New(se.Error()))
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}
