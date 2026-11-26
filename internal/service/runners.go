package service

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// Runner kinds (#47). A check whose runner is "agent:browser" goes only to
// agents that serve browser jobs. Which kinds an agent serves is a property
// of the agent its person sets (vx agent runner NAME --kinds browser,security),
// like its scope: the agent cannot change it, so a person decides which of
// their agents run (and report) which checks.

// runnerKindPattern is what a runner kind looks like.
var runnerKindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// MaxRunnerKinds bounds the kinds one agent may serve.
const MaxRunnerKinds = 20

// RunnerView is an agent with the runner kinds it serves.
type RunnerView struct {
	Agent store.Principal
	Kinds []string
}

// cleanRunnerKinds trims, deduplicates and checks kinds; "agent:browser"
// is taken as "browser". "ci" and "human" are not kinds of agents.
func cleanRunnerKinds(kinds []string) ([]string, error) {
	out := []string{}
	for _, k := range kinds {
		k = strings.TrimPrefix(strings.TrimSpace(k), "agent:")
		if k == "" || slices.Contains(out, k) {
			continue
		}
		if k == "ci" || k == "human" {
			return nil, errf(CodeInvalid, "%q is not a kind of agent: ci jobs go to CI tokens (vx repo ci-token create) and human jobs to people", k)
		}
		if !runnerKindPattern.MatchString(k) {
			return nil, errf(CodeInvalid, "runner kind %q: use lowercase letters, digits, dash or underscore, like browser or security", k)
		}
		out = append(out, k)
	}
	if len(out) > MaxRunnerKinds {
		return nil, errf(CodeInvalid, "%d runner kinds; keep it to %d", len(out), MaxRunnerKinds)
	}
	sort.Strings(out)
	return out, nil
}

// GetAgentRunner returns the runner kinds an agent serves. The agent itself
// (agent empty) and its person may read them.
func (s *Service) GetAgentRunner(ctx context.Context, actor Actor, agent string) (RunnerView, error) {
	target, err := s.scopeTarget(ctx, actor, agent)
	if err != nil {
		return RunnerView{}, err
	}
	if target.ID != actor.ID && !mayManage(actor, target) {
		return RunnerView{}, errf(CodePermission, "%s cannot see the runner kinds of %s: only the agent itself and its person can", actor.Ref(), target.Ref())
	}
	kinds, err := s.Store.AgentRunnerKinds(ctx, target.ID)
	return RunnerView{Agent: target, Kinds: kinds}, err
}

// SetAgentRunner sets the runner kinds an agent serves; none stops it from
// taking agent jobs. Only the agent's person may, never an agent (not even
// for itself) and never a superadmin for someone else's agent.
func (s *Service) SetAgentRunner(ctx context.Context, actor Actor, agent string, kinds []string) (RunnerView, error) {
	if actor.Kind != store.KindHuman {
		name := agent
		if strings.TrimSpace(name) == "" {
			name = agentName(actor.Handle)
		}
		return RunnerView{}, errf(CodePermission, "%s cannot change which checks agents run, not even its own: only the agent's person can. Ask %s to run: vx agent runner %s --kinds ...", actor.Ref(), ownerRef(actor), name)
	}
	if strings.TrimSpace(agent) == "" {
		return RunnerView{}, errf(CodeInvalid, "name the agent: vx agent runner NAME --kinds browser (vx agent list shows yours)")
	}
	target, err := s.scopeTarget(ctx, actor, agent)
	if err != nil {
		return RunnerView{}, err
	}
	if !mayManage(actor, target) {
		return RunnerView{}, errf(CodePermission, "%s cannot change the runner kinds of %s: only %s can", actor.Ref(), target.Ref(), parentRefOf(ctx, s, target))
	}
	clean, err := cleanRunnerKinds(kinds)
	if err != nil {
		return RunnerView{}, err
	}
	before, err := s.Store.AgentRunnerKinds(ctx, target.ID)
	if err != nil {
		return RunnerView{}, err
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetAgentRunnerKinds(ctx, target.ID, clean, actor.ID, s.now()); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "agent.runner", target.Ref(), map[string]any{"kinds": before}, map[string]any{"kinds": clean})
	})
	if err != nil {
		return RunnerView{}, classify(err)
	}
	return RunnerView{Agent: target, Kinds: clean}, nil
}
