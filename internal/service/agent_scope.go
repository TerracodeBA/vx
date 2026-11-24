package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// Agent scopes and budgets (#5).
//
// A person limits each of their agents to some paths (allow and deny
// globs, deny winning) and gives it budgets: lands per day, changed lines
// and files per land, open workspaces at once. vx checks them on the
// server, after the repository role: a scope only ever narrows what the
// agent's person may do, it never grants anything. No scope (the default)
// means no limits.

// Limits on scope values, so a typo cannot store something absurd.
const (
	MaxScopeGlobs  = 100
	MaxBudgetValue = 1_000_000
)

// AgentView is an agent with its scope and how much of its budgets it has
// used today.
type AgentView struct {
	Agent store.Principal
	Owner store.Principal
	Scope store.AgentScope
	// UpdatedBy is the ref of whoever last set the scope; empty: never set.
	UpdatedBy string
	// LandsToday counts the agent's lands since the start of the UTC day.
	LandsToday int
	// OpenWorkspaces counts its open workspaces in open efforts.
	OpenWorkspaces int
	// DayStart and ResetsAt bound the current UTC day the lands budget
	// counts.
	DayStart, ResetsAt time.Time
}

// LandsLeft is how many lands the agent has left today; -1: no limit.
func (v AgentView) LandsLeft() int {
	if v.Scope.MaxLandsPerDay == 0 {
		return -1
	}
	return max(0, v.Scope.MaxLandsPerDay-v.LandsToday)
}

// WorkspacesLeft is how many more workspaces the agent may open; -1: no
// limit.
func (v AgentView) WorkspacesLeft() int {
	if v.Scope.MaxWorkspaces == 0 {
		return -1
	}
	return max(0, v.Scope.MaxWorkspaces-v.OpenWorkspaces)
}

// ScopeInput changes an agent's scope. Fields left nil (or Set* false) keep
// their value; Clear removes every limit first.
type ScopeInput struct {
	Clear          bool
	SetAllow       bool
	Allow          []string
	SetDeny        bool
	Deny           []string
	MaxLandsPerDay *int
	MaxLines       *int
	MaxFiles       *int
	MaxWorkspaces  *int
}

// dayBounds returns the UTC day t falls in.
func dayBounds(t time.Time) (time.Time, time.Time) {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

// scopeTarget resolves the agent a scope command is about: the caller when
// agent is empty, else a name ("backend", one of the caller's person's
// agents) or a handle ("alice/backend").
func (s *Service) scopeTarget(ctx context.Context, actor Actor, agent string) (store.Principal, error) {
	target, err := s.tokenOwner(ctx, actor, agent)
	if err != nil {
		return store.Principal{}, err
	}
	if target.Kind != store.KindAgent {
		if strings.TrimSpace(agent) == "" {
			return store.Principal{}, errf(CodeInvalid, "%s is a %s; scopes and budgets are for agents. Name one: vx agent show NAME (vx agent list shows yours)", target.Ref(), target.Kind)
		}
		return store.Principal{}, errf(CodeInvalid, "%s is a %s; scopes and budgets are for agents", target.Ref(), target.Kind)
	}
	return target, nil
}

// GetAgent returns an agent with its scope and today's usage. An agent may
// read its own (agent empty); a person their agents'; a superadmin
// anyone's, by handle (a scope only narrows the person's role, so it gives
// a superadmin no access to repositories).
func (s *Service) GetAgent(ctx context.Context, actor Actor, agent string) (AgentView, error) {
	target, err := s.scopeTarget(ctx, actor, agent)
	if err != nil {
		return AgentView{}, err
	}
	if target.ID != actor.ID && !mayManage(actor, target) && !(actor.Kind == store.KindHuman && s.IsSuperadmin(ctx, actor)) {
		return AgentView{}, errf(CodePermission, "%s cannot see the scope of %s: only an agent itself, its person and superadmins can", actor.Ref(), target.Ref())
	}
	return s.agentView(ctx, target)
}

func (s *Service) agentView(ctx context.Context, agent store.Principal) (AgentView, error) {
	v := AgentView{Agent: agent}
	if agent.ParentID != nil {
		owner, err := s.Store.PrincipalByID(ctx, *agent.ParentID)
		if err != nil {
			return AgentView{}, classify(err)
		}
		v.Owner = owner
	}
	sc, err := s.Store.AgentScope(ctx, agent.ID)
	if err != nil {
		return AgentView{}, err
	}
	v.Scope = sc
	if sc.UpdatedBy != nil {
		v.UpdatedBy = s.principalRef(ctx, *sc.UpdatedBy)
	}
	v.DayStart, v.ResetsAt = dayBounds(s.Now())
	if v.LandsToday, err = s.Store.CountLands(ctx, agent.Ref(), v.DayStart, v.ResetsAt); err != nil {
		return AgentView{}, err
	}
	open, err := s.Store.OpenWorkspacesOf(ctx, agent.ID)
	if err != nil {
		return AgentView{}, err
	}
	v.OpenWorkspaces = len(open)
	return v, nil
}

// SetAgentScope changes an agent's path scope and budgets. Only the agent's
// person and superadmins may (budgets stop a runaway agent); an agent cannot change any scope, not even
// its own, or a scope would limit nothing.
func (s *Service) SetAgentScope(ctx context.Context, actor Actor, agent string, in ScopeInput) (AgentView, error) {
	if actor.Kind != store.KindHuman {
		name := agent
		if strings.TrimSpace(name) == "" {
			name = agentName(actor.Handle)
		}
		return AgentView{}, errf(CodePermission, "%s cannot change scopes, not even its own: only the agent's person or a superadmin can. Ask %s to run: vx agent scope %s ...", actor.Ref(), ownerRef(actor), name)
	}
	if strings.TrimSpace(agent) == "" {
		return AgentView{}, errf(CodeInvalid, "name the agent: vx agent scope NAME ... (vx agent list shows yours)")
	}
	target, err := s.scopeTarget(ctx, actor, agent)
	if err != nil {
		return AgentView{}, err
	}
	if !mayManage(actor, target) && !s.IsSuperadmin(ctx, actor) {
		return AgentView{}, errf(CodePermission, "%s cannot change the scope of %s: only %s and superadmins can", actor.Ref(), target.Ref(), parentRefOf(ctx, s, target))
	}
	cur, err := s.Store.AgentScope(ctx, target.ID)
	if err != nil {
		return AgentView{}, err
	}
	before := scopeLog(cur)
	if in.Clear {
		cur = store.AgentScope{AgentID: target.ID}
	}
	if in.SetAllow {
		if cur.Allow, err = cleanGlobs("--allow", in.Allow); err != nil {
			return AgentView{}, err
		}
	}
	if in.SetDeny {
		if cur.Deny, err = cleanGlobs("--deny", in.Deny); err != nil {
			return AgentView{}, err
		}
	}
	for _, b := range []struct {
		flag string
		v    *int
		dst  *int
	}{
		{"--max-lands-per-day", in.MaxLandsPerDay, &cur.MaxLandsPerDay},
		{"--max-lines", in.MaxLines, &cur.MaxLines},
		{"--max-files", in.MaxFiles, &cur.MaxFiles},
		{"--max-workspaces", in.MaxWorkspaces, &cur.MaxWorkspaces},
	} {
		if b.v == nil {
			continue
		}
		if *b.v < 0 || *b.v > MaxBudgetValue {
			return AgentView{}, errf(CodeInvalid, "%s %d: give a number from 1 to %d, or 0 for no limit", b.flag, *b.v, MaxBudgetValue)
		}
		*b.dst = *b.v
	}
	cur.AgentID, cur.UpdatedBy, cur.UpdatedAt = target.ID, &actor.ID, s.Now()
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetAgentScope(ctx, cur); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "agent.scope", target.Ref(), before, scopeLog(cur))
	})
	if err != nil {
		return AgentView{}, classify(err)
	}
	return s.agentView(ctx, target)
}

func parentRefOf(ctx context.Context, s *Service, agent store.Principal) string {
	if agent.ParentID == nil {
		return "its person"
	}
	return s.principalRef(ctx, *agent.ParentID)
}

func scopeLog(sc store.AgentScope) map[string]any {
	return map[string]any{"allow": sc.Allow, "deny": sc.Deny, "max_lands_per_day": sc.MaxLandsPerDay,
		"max_lines": sc.MaxLines, "max_files": sc.MaxFiles, "max_workspaces": sc.MaxWorkspaces}
}

// cleanGlobs trims, deduplicates and checks globs with the vx.toml glob
// rules, so a scope means the same thing as a rule in vx.toml.
func cleanGlobs(flag string, globs []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		if _, err := rules.CompileGlob(g); err != nil {
			return nil, errf(CodeInvalid, "%s: %v", flag, err)
		}
		seen[g] = true
		out = append(out, g)
	}
	if len(out) > MaxScopeGlobs {
		return nil, errf(CodeInvalid, "%s: %d globs; keep it to %d", flag, len(out), MaxScopeGlobs)
	}
	return out, nil
}

// scopeVerdict says why a path is outside a scope: "" when it is inside,
// else the deny glob that matches it or "outside" when no allow glob does.
func scopeVerdict(sc store.AgentScope, path string) string {
	for _, g := range sc.Deny {
		if rules.MatchGlob(g, path) {
			return g
		}
	}
	if len(sc.Allow) == 0 {
		return ""
	}
	for _, g := range sc.Allow {
		if rules.MatchGlob(g, path) {
			return ""
		}
	}
	return "outside"
}

// InScope reports whether an agent's scope lets it change path.
func InScope(sc store.AgentScope, path string) bool { return scopeVerdict(sc, path) == "" }

// scopeError refuses paths outside the scope, naming them and the command
// the agent's person runs to widen it.
func scopeError(actor Actor, sc store.AgentScope, paths []string) error {
	var outside, denied []string
	var denyGlob string
	for _, p := range paths {
		switch v := scopeVerdict(sc, p); v {
		case "":
		case "outside":
			outside = append(outside, p)
		default:
			denied = append(denied, p)
			if denyGlob == "" {
				denyGlob = v
			}
		}
	}
	if len(outside) == 0 && len(denied) == 0 {
		return nil
	}
	name := agentName(actor.Handle)
	var b strings.Builder
	if len(sc.Allow) > 0 {
		fmt.Fprintf(&b, "%s may only change %s", actor.Ref(), strings.Join(sc.Allow, ", "))
	} else {
		fmt.Fprintf(&b, "%s may change any path except %s", actor.Ref(), strings.Join(sc.Deny, ", "))
	}
	if len(sc.Allow) > 0 && len(sc.Deny) > 0 {
		fmt.Fprintf(&b, " except %s", strings.Join(sc.Deny, ", "))
	}
	if len(outside) > 0 {
		fmt.Fprintf(&b, "; %s %s outside", listPaths(outside), isAre(len(outside)))
	}
	if len(denied) > 0 {
		fmt.Fprintf(&b, "; %s %s denied by %s", listPaths(denied), isAre(len(denied)), denyGlob)
	}
	fmt.Fprintf(&b, ". Ask %s to widen it: vx agent scope %s %s", ownerRef(actor), name, widenFlags(sc, outside, denyGlob))
	return errf(CodePermission, "%s", b.String())
}

// widenFlags is the vx agent scope flags that would let the refused paths
// through. --allow and --deny replace their whole list, so the globs that
// stay are repeated.
func widenFlags(sc store.AgentScope, outside []string, denyGlob string) string {
	var flags []string
	if len(outside) > 0 {
		globs := append([]string{}, sc.Allow...)
		seen := map[string]bool{}
		for _, p := range outside {
			if g := suggestGlob(p); !seen[g] {
				seen[g] = true
				globs = append(globs, g)
			}
		}
		for _, g := range globs {
			flags = append(flags, "--allow "+shellQuote(g))
		}
	}
	if denyGlob != "" {
		kept := 0
		for _, g := range sc.Deny {
			if g != denyGlob {
				flags = append(flags, "--deny "+shellQuote(g))
				kept++
			}
		}
		if kept == 0 {
			flags = append(flags, "--deny ''")
		}
	}
	return strings.Join(flags, " ")
}

// shellQuote quotes a glob so a shell does not expand it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// suggestGlob proposes a glob covering path: everything in its directory.
func suggestGlob(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i] + "/**"
	}
	return path
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// listPaths names up to three paths and counts the rest.
func listPaths(paths []string) string {
	const show = 3
	if len(paths) <= show {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:show], ", "), len(paths)-show)
}

// limitedScope returns the actor's scope when it is an agent with limits.
func (s *Service) limitedScope(ctx context.Context, actor Actor) (store.AgentScope, bool, error) {
	if actor.Kind != store.KindAgent {
		return store.AgentScope{}, false, nil
	}
	sc, err := s.Store.AgentScope(ctx, actor.ID)
	if err != nil {
		return store.AgentScope{}, false, err
	}
	return sc, !sc.Unlimited(), nil
}

// checkAgentLand checks an agent's land of the change from tree from to
// tree to (its snapshot on its base) against its scope and budgets: lands
// left today, every changed path in scope, changed files and lines per
// land. The snapshot is the agent's own work; when the land merges onto
// newer work, what lands is a part of it, so the check covers that too.
// It does nothing for people and for agents without limits. Land calls it
// after authorization and before the landing policies.
func (s *Service) checkAgentLand(ctx context.Context, actor Actor, from, to hash.Hash) error {
	sc, limited, err := s.limitedScope(ctx, actor)
	if err != nil || !limited {
		return err
	}
	if err := s.checkLandsToday(ctx, s.Store.Q, actor, sc); err != nil {
		return err
	}
	if len(sc.Allow) == 0 && len(sc.Deny) == 0 && sc.MaxFiles == 0 && sc.MaxLines == 0 {
		return nil
	}
	changes, err := object.DiffTrees(ctx, s.Blobs, from, to)
	if err != nil {
		return err
	}
	return s.checkAgentChanges(ctx, actor, sc, changes)
}

// checkAgentChanges checks changed paths, files and lines against a scope.
func (s *Service) checkAgentChanges(ctx context.Context, actor Actor, sc store.AgentScope, changes []object.Change) error {
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	if err := scopeError(actor, sc, paths); err != nil {
		return err
	}
	name := agentName(actor.Handle)
	if sc.MaxFiles > 0 && len(changes) > sc.MaxFiles {
		return errf(CodeTooMany, "this land changes %d files and %s may change at most %d per land: split it into smaller lands, or ask %s to raise it: vx agent scope %s --max-files N",
			len(changes), actor.Ref(), sc.MaxFiles, ownerRef(actor), name)
	}
	if sc.MaxLines > 0 {
		files, err := s.lineStats(ctx, changes)
		if err != nil {
			return err
		}
		lines := 0
		for _, f := range files {
			lines += f.Added + f.Deleted
		}
		if lines > sc.MaxLines {
			return errf(CodeTooMany, "this land changes %d lines (added plus deleted) and %s may change at most %d per land: split it into smaller lands, or ask %s to raise it: vx agent scope %s --max-lines N",
				lines, actor.Ref(), sc.MaxLines, ownerRef(actor), name)
		}
	}
	return nil
}

// checkLandsToday refuses a land when the agent used up today's lands.
func (s *Service) checkLandsToday(ctx context.Context, q store.Q, actor Actor, sc store.AgentScope) error {
	if sc.MaxLandsPerDay == 0 {
		return nil
	}
	now := s.Now()
	start, end := dayBounds(now)
	n, err := q.CountLands(ctx, actor.Ref(), start, end)
	if err != nil {
		return err
	}
	if n < sc.MaxLandsPerDay {
		return nil
	}
	return errf(CodeTooMany, "%s has used all %d of its lands for today (0 left); the budget resets at %s (in %s). Ask %s to raise it: vx agent scope %s --max-lands-per-day N",
		actor.Ref(), sc.MaxLandsPerDay, end.Format("2006-01-02 15:04 UTC"), untilText(end.Sub(now)), ownerRef(actor), agentName(actor.Handle))
}

// untilText shows a wait to the minute: "9h05m" or "42m".
func untilText(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", max(m, 1))
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// recheckLandsToday repeats the lands budget check inside the land's
// transaction, with the agent's row locked, so parallel lands of one agent
// cannot together go over it.
func (s *Service) recheckLandsToday(ctx context.Context, tx *store.Tx, actor Actor) error {
	if actor.Kind != store.KindAgent {
		return nil
	}
	sc, err := tx.AgentScope(ctx, actor.ID)
	if err != nil || sc.MaxLandsPerDay == 0 {
		return err
	}
	if err := tx.LockPrincipal(ctx, actor.ID); err != nil {
		return err
	}
	return s.checkLandsToday(ctx, tx.Q, actor, sc)
}

// checkOpenWorkspaces refuses a new workspace when the agent has as many
// open as its budget allows. It runs in the transaction that opens one,
// with the agent's row locked.
func (s *Service) checkOpenWorkspaces(ctx context.Context, tx *store.Tx, actor Actor) error {
	if actor.Kind != store.KindAgent {
		return nil
	}
	sc, err := tx.AgentScope(ctx, actor.ID)
	if err != nil || sc.MaxWorkspaces == 0 {
		return err
	}
	if err := tx.LockPrincipal(ctx, actor.ID); err != nil {
		return err
	}
	open, err := tx.OpenWorkspacesOf(ctx, actor.ID)
	if err != nil {
		return err
	}
	if len(open) < sc.MaxWorkspaces {
		return nil
	}
	oldest := open[0]
	return errf(CodeTooMany, "%s already has %d open workspace%s and may have at most %d at once (0 left): land or abandon one first (the oldest is %q, change %s), or ask %s to raise it: vx agent scope %s --max-workspaces N",
		actor.Ref(), len(open), plural(len(open)), sc.MaxWorkspaces, oldest.Intent, oldest.ChangeID, ownerRef(actor), agentName(actor.Handle))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// checkRebaseResolution checks the files an agent wrote itself when it
// resolved a stopped rebase: those whose resolved content is neither the
// replayed change's (change) nor the rebased chain's (onto). Taking either
// side as it is changes nothing the agent wrote, so only its own edits must
// be in its scope.
func (s *Service) checkRebaseResolution(ctx context.Context, actor Actor, change, onto, resolved hash.Hash) error {
	sc, limited, err := s.limitedScope(ctx, actor)
	if err != nil || !limited || (len(sc.Allow) == 0 && len(sc.Deny) == 0) {
		return err
	}
	fromOnto, err := object.DiffTrees(ctx, s.Blobs, onto, resolved)
	if err != nil {
		return err
	}
	fromChange, err := object.DiffTrees(ctx, s.Blobs, change, resolved)
	if err != nil {
		return err
	}
	differs := map[string]bool{}
	for _, c := range fromChange {
		differs[c.Path] = true
	}
	var written []string
	for _, c := range fromOnto {
		if differs[c.Path] {
			written = append(written, c.Path)
		}
	}
	return scopeError(actor, sc, written)
}
