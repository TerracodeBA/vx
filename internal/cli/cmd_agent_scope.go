package cli

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// scopeErr explains a server without agent scopes instead of "404 Not Found".
func (a *App) scopeErr(err error) error {
	if client.Code(err) == connect.CodeUnimplemented {
		return a.featureErr("agent-scopes", "agent scopes and budgets", err)
	}
	return err
}

func (a *App) agentScopeCmd() *cobra.Command {
	var allow, deny []string
	var lands, lines, files, workspaces int
	var clear bool
	cmd := &cobra.Command{
		Use:   "scope NAME [--allow GLOB]... [--deny GLOB]... [--max-lands-per-day N] [--max-lines N] [--max-files N] [--max-workspaces N] [--clear]",
		Short: "Limit the paths one of your agents may change, and how much",
		Long: `Limit the paths one of your agents may change and give it budgets. vx
checks them on the server on every land, also when a land merges onto
newer work and when the agent resolves a rebase conflict.

  --allow GLOB   paths it may change (repeat for several); none: every path
                 you may change. Globs work as in vx.toml: *, ? and **.
  --deny GLOB    paths it may not change, even when an --allow matches
  --max-lands-per-day N   lands per UTC day
  --max-lines N           changed lines (added plus deleted) per land
  --max-files N           changed files per land
  --max-workspaces N      open workspaces at once
  --clear        remove every limit first (then the other flags apply)

--allow and --deny replace their whole list; --deny '' empties it. Budgets
of 0 mean no limit. Flags you leave out keep their value. Without flags it
shows the scope, like vx agent show.

A scope only narrows what you may do: the agent still needs a role on the
repository. Only you (the agent's person) and superadmins can change it;
the agent itself cannot. Name an agent of someone else by handle
(alice/backend), as a superadmin.`,
		Example: `  vx agent scope backend --allow 'internal/api/**' --allow 'docs/api.md' --deny 'internal/api/gen/**'
  vx agent scope backend --max-lands-per-day 20 --max-lines 400 --max-files 15 --max-workspaces 2
  vx agent scope backend --clear`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			f := cmd.Flags()
			req := &vxv1.SetAgentScopeRequest{Agent: args[0], Clear: clear}
			if f.Changed("allow") {
				req.SetAllow, req.Allow = true, allow
			}
			if f.Changed("deny") {
				req.SetDeny, req.Deny = true, deny
			}
			for _, b := range []struct {
				flag string
				v    int
				dst  **int32
			}{
				{"max-lands-per-day", lands, &req.MaxLandsPerDay},
				{"max-lines", lines, &req.MaxLines},
				{"max-files", files, &req.MaxFiles},
				{"max-workspaces", workspaces, &req.MaxWorkspaces},
			} {
				if f.Changed(b.flag) {
					v := int32(b.v)
					*b.dst = &v
				}
			}
			changes := clear || req.SetAllow || req.SetDeny || req.MaxLandsPerDay != nil || req.MaxLines != nil || req.MaxFiles != nil || req.MaxWorkspaces != nil
			if !changes {
				resp, err := c.Auth.GetAgent(ctxOf(cmd), connect.NewRequest(&vxv1.GetAgentRequest{Agent: args[0]}))
				if err != nil {
					return a.scopeErr(err)
				}
				a.printAgentInfo(resp.Msg.Agent)
				a.emit(resp.Msg)
				return nil
			}
			resp, err := c.Auth.SetAgentScope(ctxOf(cmd), connect.NewRequest(req))
			if err != nil {
				return a.scopeErr(err)
			}
			a.printf("Updated the scope of %s.\n\n", resp.Msg.Agent.Agent.Ref)
			a.printAgentInfo(resp.Msg.Agent)
			a.emit(resp.Msg)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&allow, "allow", nil, "glob of paths the agent may change (repeatable; replaces the list)")
	f.StringArrayVar(&deny, "deny", nil, "glob of paths the agent may not change, winning over --allow (repeatable; replaces the list)")
	f.IntVar(&lands, "max-lands-per-day", 0, "lands per UTC day (0: no limit)")
	f.IntVar(&lines, "max-lines", 0, "changed lines per land (0: no limit)")
	f.IntVar(&files, "max-files", 0, "changed files per land (0: no limit)")
	f.IntVar(&workspaces, "max-workspaces", 0, "open workspaces at once (0: no limit)")
	f.BoolVar(&clear, "clear", false, "remove every limit first")
	return cmd
}

func (a *App) agentShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [NAME]",
		Short: "Show an agent's path scope, budgets and what is left of them today",
		Long: `Show an agent's path scope, its budgets and how much of them it used
today. Without NAME, an agent shows its own. People see their own agents'
(by name, backend, or handle, alice/backend); superadmins anyone's.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			resp, err := c.Auth.GetAgent(ctxOf(cmd), connect.NewRequest(&vxv1.GetAgentRequest{Agent: name}))
			if err != nil {
				return a.scopeErr(err)
			}
			a.printAgentInfo(resp.Msg.Agent)
			a.emit(resp.Msg)
			return nil
		},
	}
}

// printAgentInfo prints an agent's scope and budgets.
func (a *App) printAgentInfo(ai *vxv1.AgentInfo) {
	if ai == nil {
		return
	}
	a.printf("%s", ai.Agent.Ref)
	if ai.Agent.OnBehalfOf != "" {
		a.printf(" (acting for %s)", ai.Agent.OnBehalfOf)
	}
	a.printf("\n")
	for _, l := range scopeLines(ai) {
		a.printf("  %s\n", l)
	}
	if sc := ai.Scope; sc != nil && sc.UpdatedBy != "" {
		a.printf("  Set by %s on %s\n", sc.UpdatedBy, when(sc.UpdatedAt, ""))
	}
}

// scopeLines describes an agent's scope and budgets in a few lines, for
// vx agent show, vx whoami and vx status.
func scopeLines(ai *vxv1.AgentInfo) []string {
	sc := ai.Scope
	if sc == nil {
		sc = &vxv1.AgentScope{}
	}
	if ai.Unlimited {
		return []string{"Scope: no limits (every path its person may change, no budgets)"}
	}
	var out []string
	switch {
	case len(sc.Allow) == 0 && len(sc.Deny) == 0:
		out = append(out, "Paths: every path its person may change")
	case len(sc.Allow) == 0:
		out = append(out, "Paths: every path its person may change, except "+strings.Join(sc.Deny, ", "))
	case len(sc.Deny) == 0:
		out = append(out, "Paths: only "+strings.Join(sc.Allow, ", "))
	default:
		out = append(out, "Paths: only "+strings.Join(sc.Allow, ", ")+"; except "+strings.Join(sc.Deny, ", "))
	}
	lands := "no limit"
	if sc.MaxLandsPerDay > 0 {
		lands = fmt.Sprintf("%d of %d left today (%d landed), resets %s", ai.LandsLeft, sc.MaxLandsPerDay, ai.LandsToday, when(ai.ResetsAt, ""))
	}
	out = append(out, "Lands: "+lands)
	ws := fmt.Sprintf("%d open, no limit", ai.OpenWorkspaces)
	if sc.MaxWorkspaces > 0 {
		ws = fmt.Sprintf("%d of %d open (%d left)", ai.OpenWorkspaces, sc.MaxWorkspaces, ai.WorkspacesLeft)
	}
	out = append(out, "Workspaces: "+ws)
	per := []string{}
	if sc.MaxLines > 0 {
		per = append(per, fmt.Sprintf("at most %d changed lines", sc.MaxLines))
	}
	if sc.MaxFiles > 0 {
		per = append(per, fmt.Sprintf("at most %d files", sc.MaxFiles))
	}
	if len(per) > 0 {
		out = append(out, "Per land: "+strings.Join(per, ", "))
	} else {
		out = append(out, "Per land: no limit on lines or files")
	}
	return out
}

// ownAgent is the caller's scope and budgets when it is an agent; nil for
// people and for servers without scopes.
func (a *App) ownAgent(ctx context.Context, c *client.Client) *vxv1.AgentInfo {
	resp, err := c.Auth.WhoAmI(ctx, connect.NewRequest(&vxv1.WhoAmIRequest{}))
	if err != nil {
		return nil
	}
	return resp.Msg.Agent
}

// outsideScope lists the paths a scope does not let its agent change, as
// the server decides it: a deny glob wins, then an allow glob must match
// when there are any.
func outsideScope(sc *vxv1.AgentScope, paths []string) []string {
	if sc == nil {
		return nil
	}
	var out []string
	for _, p := range paths {
		if !inScope(sc, p) {
			out = append(out, p)
		}
	}
	return out
}

func inScope(sc *vxv1.AgentScope, path string) bool {
	for _, g := range sc.Deny {
		if rules.MatchGlob(g, path) {
			return false
		}
	}
	if len(sc.Allow) == 0 {
		return true
	}
	for _, g := range sc.Allow {
		if rules.MatchGlob(g, path) {
			return true
		}
	}
	return false
}

// precheckScope refuses a land that the server would refuse for the
// agent's scope or lands budget, before the checks run for nothing. The
// server checks again; this only saves the wait.
func (a *App) precheckScope(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) error {
	ai := a.ownAgent(ctx, c)
	if ai == nil || ai.Unlimited || ai.Scope == nil {
		return nil
	}
	sc := ai.Scope
	who := ai.Agent.Ref
	owner := ai.Agent.OnBehalfOf
	name := who[strings.LastIndex(who, "/")+1:]
	if sc.MaxLandsPerDay > 0 && ai.LandsLeft == 0 {
		return fmt.Errorf("%s has used all %d of its lands for today; the budget resets %s. Ask %s to raise it: vx agent scope %s --max-lands-per-day N",
			who, sc.MaxLandsPerDay, when(ai.ResetsAt, ""), owner, name)
	}
	changes, err := wc.Status(ctx)
	if err != nil {
		return err
	}
	paths := make([]string, len(changes))
	for i, ch := range changes {
		paths[i] = ch.Path
	}
	if out := outsideScope(sc, paths); len(out) > 0 {
		may := "may change any path"
		if len(sc.Allow) > 0 {
			may = "may only change " + strings.Join(sc.Allow, ", ")
		}
		if len(sc.Deny) > 0 {
			may += " except " + strings.Join(sc.Deny, ", ")
		}
		verb := "is"
		if len(out) > 1 {
			verb = "are"
		}
		return fmt.Errorf("permission denied: %s %s; %s %s outside it. Nothing ran or landed: revert those files, or ask %s to widen it: vx agent scope %s --allow ...",
			who, may, strings.Join(out, ", "), verb, owner, name)
	}
	if sc.MaxFiles > 0 && len(changes) > int(sc.MaxFiles) {
		return fmt.Errorf("this land changes %d files and %s may change at most %d per land; nothing ran or landed. Split it into smaller lands, or ask %s: vx agent scope %s --max-files N",
			len(changes), who, sc.MaxFiles, owner, name)
	}
	return nil
}
