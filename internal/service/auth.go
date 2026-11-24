package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

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
		tok, secret, err := tx.IssueToken(ctx, agent.ID, "created by "+actor.Ref())
		if err != nil {
			return err
		}
		token = secret
		// Single sign-on carries over from the token that made it.
		if err := tx.CopyTokenSSO(ctx, actor.TokenID, tok.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "agent.create", agent.Ref(), nil, nil)
	})
	return agent, token, classify(err)
}

// AgentSummary is one of a person's agents with a summary of its tokens.
type AgentSummary struct {
	Agent         store.Principal
	ActiveTokens  int
	RevokedTokens int
	LastUsedAt    *time.Time // latest use of any of its tokens
}

// ListAgents returns the agents acting for the calling person.
func (s *Service) ListAgents(ctx context.Context, actor Actor) ([]AgentSummary, error) {
	if actor.Kind != store.KindHuman {
		return nil, errf(CodePermission, "only people have agents; %s is a %s. Ask %s to run `vx agent list`", actor.Ref(), actor.Kind, ownerRef(actor))
	}
	agents, err := s.Store.ListAgents(ctx, actor.ID)
	if err != nil || len(agents) == 0 {
		return nil, classify(err)
	}
	ids := make([]uuid.UUID, len(agents))
	byID := map[uuid.UUID]*AgentSummary{}
	out := make([]AgentSummary, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
		out[i] = AgentSummary{Agent: a}
		byID[a.ID] = &out[i]
	}
	tokens, err := s.Store.ListTokens(ctx, ids...)
	if err != nil {
		return nil, err
	}
	for _, t := range tokens {
		sum := byID[t.PrincipalID]
		if t.Active() {
			sum.ActiveTokens++
		} else {
			sum.RevokedTokens++
		}
		if t.LastUsedAt != nil && (sum.LastUsedAt == nil || t.LastUsedAt.After(*sum.LastUsedAt)) {
			sum.LastUsedAt = t.LastUsedAt
		}
	}
	return out, nil
}

func ownerRef(a Actor) string {
	if a.Parent != nil {
		return a.Parent.Ref()
	}
	return "its owner"
}

// mayManage reports whether actor may see and revoke p's tokens: its own,
// and for people, those of their agents.
func mayManage(actor Actor, p store.Principal) bool {
	return p.ID == actor.ID || (actor.Kind == store.KindHuman && p.ParentID != nil && *p.ParentID == actor.ID)
}

// ListTokens returns the tokens of the caller (agent empty) or of one of the
// caller's agents, given by name ("backend") or handle ("alice/backend").
func (s *Service) ListTokens(ctx context.Context, actor Actor, agent string) (store.Principal, []store.Token, error) {
	target, err := s.tokenOwner(ctx, actor, agent)
	if err != nil {
		return store.Principal{}, nil, err
	}
	if !mayManage(actor, target) {
		return store.Principal{}, nil, errf(CodePermission, "%s cannot see the tokens of %s: only your own and your agents'", actor.Ref(), target.Ref())
	}
	tokens, err := s.Store.ListTokens(ctx, target.ID)
	return target, tokens, classify(err)
}

// tokenOwner resolves the principal a token command is about: the caller
// (agent empty) or an agent given by name ("backend") or handle
// ("alice/backend"). It does not check permissions.
func (s *Service) tokenOwner(ctx context.Context, actor Actor, agent string) (store.Principal, error) {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return actor.Principal, nil
	}
	handle := agent
	if !strings.Contains(handle, "/") {
		handle = actor.Human().Handle + "/" + agent
	}
	p, err := s.Store.PrincipalByHandle(ctx, handle)
	if errors.Is(err, store.ErrNotFound) {
		return store.Principal{}, errf(CodeNotFound, "no agent %s; `vx agent list` shows yours", handle)
	}
	return p, err
}

// MaxTokenName is the longest token label CreateToken accepts.
const MaxTokenName = 100

// CreateToken issues a new token to the caller (agent empty) or to one of
// the caller's agents and returns it with its secret, which is shown once.
//
// Only people can create tokens, also for themselves. Agents can list and
// revoke their own tokens, but if an agent could mint tokens, a leaked agent
// token could create replacements that survive its revocation, so revoking
// it would no longer contain the leak. An agent that needs a new token asks
// the person it acts for.
func (s *Service) CreateToken(ctx context.Context, actor Actor, agent, name string) (store.Token, store.Principal, string, error) {
	target, err := s.TokenCreateTarget(ctx, actor, agent)
	if err != nil {
		return store.Token{}, store.Principal{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "created by " + actor.Ref()
	}
	if len([]rune(name)) > MaxTokenName {
		return store.Token{}, store.Principal{}, "", errf(CodeInvalid, "token name is %d characters; keep it to %d", len([]rune(name)), MaxTokenName)
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return store.Token{}, store.Principal{}, "", errf(CodeInvalid, "token name must be one line without tabs")
	}
	var tok store.Token
	var secret string
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if tok, secret, err = tx.IssueToken(ctx, target.ID, name); err != nil {
			return err
		}
		if err := tx.CopyTokenSSO(ctx, actor.TokenID, tok.ID); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "token.create", target.Ref(), nil,
			map[string]string{"token": tok.ID.String(), "name": tok.Name})
	})
	if err != nil {
		return store.Token{}, store.Principal{}, "", classify(err)
	}
	return tok, target, secret, nil
}

// TokenCreateTarget returns who CreateToken would create a token for, or
// why the caller may not. It changes nothing.
func (s *Service) TokenCreateTarget(ctx context.Context, actor Actor, agent string) (store.Principal, error) {
	if actor.Kind != store.KindHuman {
		return store.Principal{}, errf(CodePermission,
			"only people can create tokens; %s is an agent. Ask %s to run `vx token create --agent %s`", actor.Ref(), ownerRef(actor), agentName(actor.Handle))
	}
	target, err := s.tokenOwner(ctx, actor, agent)
	if err != nil {
		return store.Principal{}, err
	}
	if !mayManage(actor, target) {
		return store.Principal{}, errf(CodePermission, "%s cannot create tokens for %s: only for yourself and your agents", actor.Ref(), target.Ref())
	}
	return target, nil
}

// agentName is the part of an agent handle after the slash.
func agentName(handle string) string {
	if i := strings.LastIndex(handle, "/"); i >= 0 {
		return handle[i+1:]
	}
	return handle
}

// RevokeInput says which token to revoke.
type RevokeInput struct {
	// ID is a token id or a unique prefix of one (at least 8 characters)
	// among the tokens the caller may manage.
	ID string
	// AllowCurrent allows revoking the token the request is made with, which
	// signs the caller out.
	AllowCurrent bool
}

// MinTokenPrefix is the shortest token id prefix RevokeToken accepts.
const MinTokenPrefix = 8

// RevokeToken revokes one of the caller's tokens or one of their agents'.
// It returns the token and whether it was already revoked.
func (s *Service) RevokeToken(ctx context.Context, actor Actor, in RevokeInput) (store.Token, store.Principal, bool, error) {
	tok, owner, err := s.findToken(ctx, actor, strings.ToLower(strings.TrimSpace(in.ID)))
	if err != nil {
		return store.Token{}, store.Principal{}, false, err
	}
	if !tok.Active() {
		return tok, owner, true, nil
	}
	if tok.ID == actor.TokenID && !in.AllowCurrent {
		return store.Token{}, store.Principal{}, false, errf(CodeFailedPrecondition,
			"token %s is the one you are using right now; revoking it signs you out. Pass --current to revoke it anyway", tok.ID)
	}
	var revoked bool
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if tok, revoked, err = tx.RevokeToken(ctx, tok.ID); err != nil || !revoked {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "token.revoke", owner.Ref(), nil,
			map[string]string{"token": tok.ID.String(), "name": tok.Name})
	})
	if err != nil {
		return store.Token{}, store.Principal{}, false, classify(err)
	}
	return tok, owner, !revoked, nil
}

// Token returns a token the caller may manage, by id or unique prefix, with
// its owner. It changes nothing; use it to show what RevokeToken would do.
func (s *Service) Token(ctx context.Context, actor Actor, id string) (store.Token, store.Principal, error) {
	return s.findToken(ctx, actor, strings.ToLower(strings.TrimSpace(id)))
}

// findToken resolves a token id or prefix and checks the caller may manage it.
func (s *Service) findToken(ctx context.Context, actor Actor, id string) (store.Token, store.Principal, error) {
	if full, err := uuid.Parse(id); err == nil {
		tok, err := s.Store.TokenByID(ctx, full)
		if errors.Is(err, store.ErrNotFound) {
			return store.Token{}, store.Principal{}, errf(CodeNotFound, "no token %s; `vx token list` shows yours", id)
		}
		if err != nil {
			return store.Token{}, store.Principal{}, err
		}
		owner, err := s.Store.PrincipalByID(ctx, tok.PrincipalID)
		if err != nil {
			return store.Token{}, store.Principal{}, err
		}
		if !mayManage(actor, owner) {
			return store.Token{}, store.Principal{}, errf(CodePermission, "token %s belongs to %s; you can only revoke your own tokens and your agents'", id, owner.Ref())
		}
		return tok, owner, nil
	}
	if len(id) < MinTokenPrefix {
		return store.Token{}, store.Principal{}, errf(CodeInvalid, "token id %q is too short: give at least %d characters, as `vx token list` shows them", id, MinTokenPrefix)
	}
	owners := map[uuid.UUID]store.Principal{actor.ID: actor.Principal}
	if actor.Kind == store.KindHuman {
		agents, err := s.Store.ListAgents(ctx, actor.ID)
		if err != nil {
			return store.Token{}, store.Principal{}, err
		}
		for _, a := range agents {
			owners[a.ID] = a
		}
	}
	ids := make([]uuid.UUID, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	tokens, err := s.Store.ListTokens(ctx, ids...)
	if err != nil {
		return store.Token{}, store.Principal{}, err
	}
	var match []store.Token
	for _, t := range tokens {
		if strings.HasPrefix(t.ID.String(), id) {
			match = append(match, t)
		}
	}
	switch len(match) {
	case 0:
		return store.Token{}, store.Principal{}, errf(CodeNotFound, "none of your tokens or your agents' has an id starting with %s; `vx token list` shows them", id)
	case 1:
		return match[0], owners[match[0].PrincipalID], nil
	}
	return store.Token{}, store.Principal{}, errf(CodeInvalid, "%d tokens have ids starting with %s; give more characters", len(match), id)
}
