package service

import (
	"context"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// CreateAgent creates an agent that acts for the calling human and returns
// it with a fresh token. Agents cannot create agents.
func (s *Service) CreateAgent(ctx context.Context, actor Actor, name, displayName string) (store.Principal, string, error) {
	if actor.Kind != store.KindHuman {
		return store.Principal{}, "", errf(CodePermission, "only people can create agents; %s is a %s", actor.Ref(), actor.Kind)
	}
	name = strings.TrimSpace(name)
	handle := actor.Handle + "/" + name
	if err := store.ValidateHandle(store.KindAgent, handle); err != nil {
		return store.Principal{}, "", errf(CodeInvalid, "%v", err)
	}
	if displayName == "" {
		displayName = name
	}
	var agent store.Principal
	var token string
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		agent, err = tx.CreatePrincipal(ctx, store.Principal{
			Kind: store.KindAgent, Handle: handle, ParentID: &actor.ID, DisplayName: displayName, Email: actor.Email,
		})
		if err != nil {
			return err
		}
		token, err = tx.CreateToken(ctx, agent.ID, "created by "+actor.Ref())
		if err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "agent.create", agent.Ref(), nil, nil)
	})
	return agent, token, classify(err)
}
