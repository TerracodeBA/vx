package api

import (
	"context"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

func (h *Handlers) CreateLoginCode(ctx context.Context, _ *connect.Request[vxv1.CreateLoginCodeRequest]) (*connect.Response[vxv1.CreateLoginCodeResponse], error) {
	code, expires, err := h.svc.CreateLoginCode(ctx, actorFrom(ctx))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreateLoginCodeResponse{Code: code, ExpiresAt: ts(expires)}), nil
}
