package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// ---------- check: company-defined checks (#47) ----------

func (a *App) checkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Report and see the results of the checks vx.toml defines",
		Long: `Checks are what a company wants proven before work lands or ships: unit
tests, a security review, a browser test, a person's sign-off. vx.toml
defines them ([[check]], see docs/checks.md); agents, CI and people run
them and report results here. A result is bound to a change and the exact
tree it describes: a new tree needs new evidence.

  vx check status                      what this change still needs, per check
  vx check run unit                    run a run check's command here and report it
  vx check report security-review --status pass --summary "No findings" --log review.md
  vx check show 1a2b3c4d --download out/

Checks whose runner is an agent kind, ci or a person become jobs; runners
take them with a lease and report the result:

  vx check next --runner browser --wait   take a job (agents of that kind)
  vx check renew JOB                       keep the lease while you work
  vx check release JOB --reason TEXT       give it back
  vx check jobs                            the effort's queue
  vx check job JOB                         one job with its inputs
  vx check request NAME --value k=v        ask for a check to run

docs/checks.md describes checks and results, docs/runners.md runners,
leases, CI tokens and a browser runner.

Without --change, a check is about your open workspace's change with the
files as they are now (vx uploads them), or, without a workspace, about
the effort head.`,
	}
	cmd.AddCommand(a.checkStatusCmd(), a.checkReportCmd(), a.checkRunCmd(), a.checkShowCmd())
	cmd.AddCommand(a.checkQueueCmds()...)
	return cmd
}

// checkWhere says what a check command is about.
type checkWhere struct {
	repo, effort, change, tree string
	what                       string // for people: "your workspace's change kxqr-7m2p"
	wc                         *workcopy.WorkingCopy
}

type checkTargetFlags struct {
	repo, effort, change, tree string
}

func (f *checkTargetFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.repo, "repo", "", "repository (default: the working copy's)")
	cmd.Flags().StringVar(&f.effort, "effort", "", "effort slug (default: the working copy's)")
	cmd.Flags().StringVar(&f.change, "change", "", "change id (default: your open workspace's, else the effort head)")
	cmd.Flags().StringVar(&f.tree, "tree", "", "the tree the result describes (default: the change's newest; for a change not landed yet, its snapshot)")
}

// strict: refuse local changes that no workspace holds, and a working
// copy behind the effort head (a result must describe what it says).
func (a *App) resolveCheckWhere(ctx context.Context, c *client.Client, f checkTargetFlags, strict bool) (checkWhere, error) {
	w := checkWhere{change: f.change, tree: f.tree}
	wc, wcErr := a.workingCopy()
	repo, err := a.resolveRepo(f.repo)
	if err != nil {
		return w, err
	}
	w.repo = repo
	w.effort = f.effort
	if w.effort == "" {
		if wcErr != nil || wc.Config.Repo != repo {
			return w, errors.New("which effort? pass --effort or run inside a working copy")
		}
		w.effort = wc.Config.Effort
	}
	inCopy := wcErr == nil && wc.Config.Repo == repo && wc.Config.Effort == w.effort
	if inCopy {
		w.wc = wc
	}
	if w.change != "" {
		w.what = "change " + w.change
		return w, nil
	}
	if !inCopy {
		w.what = "the effort head"
		return w, nil
	}
	if wc.Config.Workspace != "" {
		tree, err := a.uploadSnapshot(ctx, c, wc)
		if err != nil {
			return w, err
		}
		w.change, w.tree = wc.Config.ChangeID, tree
		w.what = fmt.Sprintf("your workspace's change %s (your files now, tree %s)", wc.Config.ChangeID, short(tree))
		return w, nil
	}
	if strict {
		if changes, err := wc.Status(ctx); err == nil && len(changes) > 0 {
			return w, fmt.Errorf("you have %d local change(s) but no workspace, so a result would describe neither them nor the effort head: vx new \"INTENT\" first (the result is then about that change), or put them aside", len(changes))
		}
		moved, err := a.effortMoved(ctx, c, wc)
		if err != nil {
			return w, err
		}
		if moved {
			return w, errors.New("your working copy is behind the effort head: vx sync first, so the result describes the head")
		}
	}
	w.what = "the effort head"
	return w, nil
}

// uploadSnapshot snapshots the working copy and uploads what the server
// lacks of it, so the server can read the tree.
func (a *App) uploadSnapshot(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) (string, error) {
	tree, _, err := wc.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	trees, blobs, err := object.NewObjects(ctx, wc.Objects, wc.Config.BaseTreeHash(), tree)
	if err != nil {
		return "", err
	}
	if _, err := c.Upload(ctx, wc.Config.Repo, wc.Objects, append(trees, blobs...)); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return tree.String(), nil
}

func (a *App) checkStatusCmd() *cobra.Command {
	var f checkTargetFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "What a change still needs: each check, missing, pending, passed or failed, and by whom",
		Long: `List the checks of vx.toml for a change and their state on its exact tree:
missing (no result yet), pending, passed, failed or neutral, with who
reported the result that counts and how to provide a missing one. Checks
whose paths the change does not touch are listed as not needed.

Results that do not count (a reporter the check does not allow, a run
check that vx did not run) are shown with the reason.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			w, err := a.resolveCheckWhere(ctx, c, f, false)
			if err != nil {
				return err
			}
			resp, err := c.Checks.CheckStatus(ctx, connect.NewRequest(&vxv1.CheckStatusRequest{Repo: w.repo, Effort: w.effort, ChangeId: w.change, Tree: w.tree}))
			if err != nil {
				return err
			}
			if a.jsonOut {
				a.emit(resp.Msg)
				return nil
			}
			a.printCheckStatus(w, resp.Msg)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func (a *App) printCheckStatus(w checkWhere, st *vxv1.CheckStatusResponse) {
	what := w.what
	if what == "the effort head" || what == "change "+w.change {
		what = fmt.Sprintf("change %s", st.ChangeId)
		if st.Head {
			what += " (the effort head)"
		}
	}
	if strings.Contains(what, "tree ") {
		a.printf("Checks of %s:\n", what)
	} else {
		a.printf("Checks of %s, tree %s:\n", what, short(st.Tree))
	}
	if st.Problem != "" {
		a.printf("  vx.toml does not parse, so its checks are not known: %s\n", st.Problem)
	}
	for _, w := range st.Warnings {
		a.printf("  Warning: %s\n", w)
	}
	if st.EditsRules {
		a.printf("  Note: this change edits vx.toml: the checks of the effort's base apply until it lands (a check it adds applies too; one it removes or weakens still applies).\n")
	}
	var applying, other []*vxv1.CheckState
	for _, c := range st.Checks {
		if c.Applies {
			applying = append(applying, c)
		} else {
			other = append(other, c)
		}
	}
	if len(st.Checks) == 0 {
		a.printf("  No checks: vx.toml defines none ([[check]], [checks] required, [[policy]]; see docs/checks.md).\n")
	}
	// The pipeline: each check indented under the one it runs after.
	label := func(c *vxv1.CheckState) string { return strings.Repeat("  ", int(c.Depth)) + c.Def.Name }
	width := 0
	for _, c := range applying {
		if len(label(c)) > width {
			width = len(label(c))
		}
	}
	needLand, needShip := []string{}, []string{}
	for _, c := range applying {
		detail := c.How
		if c.Latest != nil {
			detail = resultLine(c.Latest)
			if c.State != "passed" {
				detail += "; " + c.How
			}
		}
		state := c.State
		switch c.PipelineState {
		case "waiting", "skipped":
			// Not run yet because of the checks it runs after: say why
			// instead of how to report it.
			state, detail = c.PipelineState, c.Reason
		}
		a.printf("  %-*s  %-8s %s%s%s\n", width, label(c), state, detail, blocksNote(c.Def), originNote(c.Origin))
		for _, n := range c.Results {
			if c.Latest != nil && n.Id == c.Latest.Id {
				break
			}
			if !n.Counts {
				a.printf("  %-*s           not counted: %s: %s\n", width, "", resultLine(n), n.Why)
			}
		}
		if c.State != "passed" {
			switch c.Def.Blocks {
			case "land":
				needLand = append(needLand, c.Def.Name)
			case "ship":
				needShip = append(needShip, c.Def.Name)
			}
		}
	}
	if len(other) > 0 {
		var names []string
		for _, c := range other {
			names = append(names, c.Def.Name)
		}
		a.printf("  Not needed (the change touches none of their paths): %s\n", strings.Join(names, ", "))
	}
	for _, r := range st.Other {
		a.printf("  Other result: %s\n", resultLine(r))
	}
	switch {
	case len(needLand) > 0 && len(needShip) > 0:
		a.printf("Still needed to land: %s. To ship: %s.\n", strings.Join(needLand, ", "), strings.Join(needShip, ", "))
	case len(needLand) > 0:
		a.printf("Still needed to land: %s.\n", strings.Join(needLand, ", "))
	case len(needShip) > 0:
		a.printf("Nothing more needed to land. To ship: %s.\n", strings.Join(needShip, ", "))
	case len(applying) > 0:
		a.printf("Nothing more needed: every check that blocks has a passing result.\n")
	}
}

// originNote says where a check comes from: "[organization acme (version
// 3)]", "[vx.toml line 12]", "[vx.toml [checks] required]".
func originNote(origin string) string {
	if origin == "" {
		return ""
	}
	return "  [from " + origin + "]"
}

func blocksNote(d *vxv1.CheckDef) string {
	switch d.Blocks {
	case "land":
		return "  [blocks land]"
	case "ship":
		return "  [blocks ship]"
	}
	return ""
}

// resultLine describes a result: "pass by agent:alice/x at 14:02 (vx ran
// `go test ./...`): 3 artifacts [1a2b3c4d]".
func resultLine(r *vxv1.CheckResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s by %s", r.Status, r.Reporter)
	if t, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil {
		b.WriteString(" at " + reusedWhen(t))
	}
	switch {
	case r.Source == "land" && r.How != "":
		fmt.Fprintf(&b, " (vx land --run `%s`)", r.How)
	case r.How != "":
		fmt.Fprintf(&b, " (vx ran `%s`)", r.How)
	case r.Source == "review":
		b.WriteString(" (approved the review)")
	}
	if r.Summary != "" {
		fmt.Fprintf(&b, ": %s", firstLine(r.Summary))
	}
	if n := len(r.Artifacts); n > 0 {
		fmt.Fprintf(&b, ", %d artifact(s)", n)
	}
	if len(r.Id) >= 8 {
		fmt.Fprintf(&b, " [%s]", r.Id[:8])
	}
	return b.String()
}

func (a *App) checkReportCmd() *cobra.Command {
	var f checkTargetFlags
	var status, summary, logFile, dataFile string
	var artifacts []string
	cmd := &cobra.Command{
		Use:   "report NAME --status pass|fail|neutral --summary TEXT",
		Short: "Report a result of a check, with its log, artifacts and data",
		Long: `Report a result of a check vx.toml defines, bound to the change and the
exact tree it describes. Logs and artifacts become objects of the
repository: everyone who can read it can see them (vx check show).

  vx check report security-review --status pass --summary "No findings" --log review.md
  vx check report browser-e2e --status fail --summary "Checkout button does nothing" \
    --artifact flow.gif --artifact step3.png --data steps.json

A passing result must include what the check's outputs.required names
(an artifact, the data file, or a key of the data), and artifact names
must match outputs.artifacts. Whether a result counts depends on the
check's reporter: self (anyone with write), independent (not the author
of the change, nor another agent of the same person), ci or human. vx says
right away whether yours counts.

Run checks (run = "CMD") count only when vx ran the command: vx check
run NAME or vx land --run CMD.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch status {
			case "pass", "fail", "neutral", "pending":
			case "":
				return errors.New("say how it went: --status pass, fail or neutral")
			default:
				return fmt.Errorf("--status %q: use pass, fail or neutral", status)
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			req := &vxv1.ReportCheckRequest{Check: args[0], Status: status, Summary: summary, Model: a.Getenv("VX_MODEL"), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			if logFile != "" {
				if req.Log, err = os.ReadFile(logFile); err != nil {
					return fmt.Errorf("--log: %w", err)
				}
			}
			for _, p := range artifacts {
				data, err := os.ReadFile(p)
				if err != nil {
					return fmt.Errorf("--artifact: %w", err)
				}
				req.Artifacts = append(req.Artifacts, &vxv1.CheckArtifact{Name: filepath.Base(p), Data: data})
			}
			if dataFile != "" {
				if req.Data, err = os.ReadFile(dataFile); err != nil {
					return fmt.Errorf("--data: %w", err)
				}
				req.DataName = filepath.Base(dataFile)
			}
			w, err := a.resolveCheckWhere(ctx, c, f, true)
			if err != nil {
				return err
			}
			req.Repo, req.Effort, req.ChangeId, req.Tree = w.repo, w.effort, w.change, w.tree
			return a.reportAndSay(ctx, c, w, req)
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&status, "status", "", "pass, fail or neutral")
	cmd.Flags().StringVar(&summary, "summary", "", "one or a few lines: what was checked and found")
	cmd.Flags().StringVar(&logFile, "log", "", "file with the full log or report")
	cmd.Flags().StringArrayVar(&artifacts, "artifact", nil, "file the check produced: a screenshot, recording, report (repeatable)")
	cmd.Flags().StringVar(&dataFile, "data", "", "JSON file with the check's structured output, e.g. steps.json")
	return cmd
}

// reportAndSay sends a report and says whether it counts.
func (a *App) reportAndSay(ctx context.Context, c *client.Client, w checkWhere, req *vxv1.ReportCheckRequest) error {
	resp, err := c.Checks.ReportCheck(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	r := resp.Msg.Result
	counts, why := "", r.Why
	if r.Counts {
		counts = "it counts"
		if st, err := c.Checks.CheckStatus(ctx, connect.NewRequest(&vxv1.CheckStatusRequest{Repo: w.repo, Effort: w.effort, ChangeId: r.ChangeId, Tree: r.Tree})); err == nil {
			for _, cs := range st.Msg.Checks {
				if cs.Def.Name == r.Check && cs.Latest != nil && cs.Latest.Id == r.Id {
					counts += ": " + r.Check + " is " + cs.State + " on this tree"
				} else if cs.Def.Name == r.Check && !cs.Applies {
					counts += " (the change touches none of its paths, so it is not needed)"
				}
			}
		}
	}
	a.printf("Reported %s for %s on change %s (tree %s) as result %s", r.Status, r.Check, r.ChangeId, short(r.Tree), r.Id[:8])
	if len(r.Artifacts) > 0 {
		a.printf(" with %d artifact(s)", len(r.Artifacts))
	}
	a.printf(".\n")
	switch {
	case counts != "":
		a.printf("  %s.\n", strings.ToUpper(counts[:1])+counts[1:])
	case why != "":
		a.printf("  It does not count: %s.\n", why)
	}
	a.emit(map[string]any{"result": r, "counts": counts != "", "why": why})
	return nil
}

func (a *App) checkRunCmd() *cobra.Command {
	var f checkTargetFlags
	var keepGoing, noReuse bool
	cmd := &cobra.Command{
		Use:   "run [NAME...]",
		Short: "Run run checks (run = \"CMD\") here and report their results",
		Long: `Run the command of each named run check ([[check]] with run = "CMD", or a
command of [checks] required) in the working copy, like vx land --run,
and report each result with its output as the log. Without names: every
run check this change needs that has no passing result yet.

In an open workspace the results describe your change with the files as
they are now, so a later vx land of the same files needs no --run for
them. Without a workspace the working copy must be at the effort head.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, wc, err := a.clientAndCopy()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			w, err := a.resolveCheckWhere(ctx, c, f, true)
			if err != nil {
				return err
			}
			if w.wc == nil || f.change != "" {
				return errors.New("vx check run runs in the working copy of the change: run it there without --change")
			}
			st, err := c.Checks.CheckStatus(ctx, connect.NewRequest(&vxv1.CheckStatusRequest{Repo: w.repo, Effort: w.effort, ChangeId: w.change, Tree: w.tree}))
			if err != nil {
				return err
			}
			var cmds, names []string
			byName := map[string]*vxv1.CheckState{}
			for _, cs := range st.Msg.Checks {
				if _, ok := byName[cs.Def.Name]; !ok {
					byName[cs.Def.Name] = cs
				}
			}
			if len(args) == 0 {
				for _, cs := range st.Msg.Checks {
					if cs.Def.Run != "" && cs.Applies && cs.State != "passed" && !contains(cmds, cs.Def.Run) {
						cmds, names = append(cmds, cs.Def.Run), append(names, cs.Def.Name)
					}
				}
				if len(cmds) == 0 {
					a.printf("No run check needs a result on %s.\n", w.what)
					a.emit(map[string]any{"ran": []string{}})
					return nil
				}
			}
			for _, n := range args {
				cs, ok := byName[n]
				switch {
				case !ok:
					return fmt.Errorf("no check %q for this change; vx check status lists them", n)
				case cs.Def.Run == "":
					return fmt.Errorf("check %q has no run command (runner %s): its runner reports it with vx check report", n, cs.Def.Runner)
				}
				cmds, names = append(cmds, cs.Def.Run), append(names, cs.Def.Name)
			}
			run, err := a.runChecks(ctx, c, wc, cmds, checkOptions{reuse: !noReuse, keepGoing: keepGoing})
			if err != nil {
				return err
			}
			if len(run.changed) > 0 {
				a.printChangedByChecks(run.changed)
				return errors.New("the checks changed files, so their results describe files you no longer have: nothing reported. Run vx check run again")
			}
			for _, ch := range run.ran {
				i := indexOf(cmds, ch.Name)
				status := "fail"
				if ch.Result == "pass" {
					status = "pass"
				}
				summary := fmt.Sprintf("%s in %.1fs", map[string]string{"pass": "passed", "fail": "failed"}[status], float64(ch.DurationMs)/1000)
				if ch.Notes != "" {
					summary += "; " + ch.Notes
				}
				req := &vxv1.ReportCheckRequest{
					Repo: w.repo, Effort: w.effort, ChangeId: w.change, Tree: w.tree, Check: names[i], Status: status,
					Summary: summary, How: ch.Name, Model: a.Getenv("VX_MODEL"),
					StartedAt:  time.Now().Add(-time.Duration(ch.DurationMs) * time.Millisecond).UTC().Format(time.RFC3339Nano),
					FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
				}
				if data, err := os.ReadFile(wc.CheckLogPath(i + 1)); err == nil {
					req.Log = tailBytes(data, checkLogMax)
				}
				if err := a.reportAndSay(ctx, c, w, req); err != nil {
					return err
				}
			}
			if run.failed {
				return errors.New("checks failed; their results are reported. Fix them and run vx check run again")
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&keepGoing, "keep-going", false, "run every command even after one failed")
	cmd.Flags().BoolVar(&noReuse, "no-reuse", false, "run every command, even one that already passed on exactly these files")
	return cmd
}

// checkLogMax bounds the log vx check run sends: the end of the output.
const checkLogMax = 4 << 20

func tailBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

func indexOf(xs []string, x string) int {
	for i, s := range xs {
		if s == x {
			return i
		}
	}
	return 0
}

func (a *App) checkShowCmd() *cobra.Command {
	var download string
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "Show one check result; --download DIR saves its log and artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			ctx := ctxOf(cmd)
			resp, err := c.Checks.GetCheckResult(ctx, connect.NewRequest(&vxv1.GetCheckResultRequest{Id: args[0]}))
			if err != nil {
				return err
			}
			r := resp.Msg.Result
			name := r.Check
			if name == "" {
				name = "(no check names it)"
			}
			a.printf("Check result %s: %s %s\n", r.Id, name, r.Status)
			a.printf("  Change:   %s, tree %s (%s, effort %s)\n", r.ChangeId, short(r.Tree), r.Repo, r.Effort)
			who := r.Reporter
			if r.OnBehalfOf != "" {
				who += " for " + r.OnBehalfOf
			}
			if r.Model != "" {
				who += ", model " + r.Model
			}
			if r.CiTokenId != "" {
				who += ", CI token " + r.CiTokenId
			}
			a.printf("  Reporter: %s (%s)\n", who, r.Source)
			if r.How != "" {
				a.printf("  How:      %s\n", r.How)
			}
			if r.StartedAt != "" || r.FinishedAt != "" {
				a.printf("  Ran:      %s to %s\n", orDash(r.StartedAt), orDash(r.FinishedAt))
			}
			a.printf("  Reported: %s\n", r.CreatedAt)
			if r.Summary != "" {
				a.printf("  Summary:  %s\n", strings.ReplaceAll(r.Summary, "\n", "\n            "))
			}
			if r.LogSize > 0 {
				a.printf("  Log:      %s\n", humanBytes(r.LogSize))
			}
			if r.DataName != "" || r.DataJson != "" {
				a.printf("  Data:     %s %s\n", r.DataName, clipText(r.DataJson, 200))
			}
			for _, x := range r.Artifacts {
				a.printf("  Artifact: %s (%s, %s)\n", x.Name, x.ContentType, humanBytes(x.Size))
			}
			var saved []string
			if download != "" {
				if err := os.MkdirAll(download, 0o755); err != nil {
					return err
				}
				names := []string{}
				if r.LogSize > 0 {
					names = append(names, "")
				}
				for _, x := range r.Artifacts {
					names = append(names, x.Name)
				}
				for _, n := range names {
					ar, err := c.Checks.GetCheckArtifact(ctx, connect.NewRequest(&vxv1.GetCheckArtifactRequest{Id: r.Id, Name: n}))
					if err != nil {
						return err
					}
					p := filepath.Join(download, filepath.Base(ar.Msg.Artifact.Name))
					if err := os.WriteFile(p, ar.Msg.Artifact.Data, 0o644); err != nil {
						return err
					}
					saved = append(saved, p)
				}
				if r.DataJson != "" {
					dn := r.DataName
					if dn == "" {
						dn = "data.json"
					}
					p := filepath.Join(download, filepath.Base(dn))
					if err := os.WriteFile(p, []byte(r.DataJson), 0o644); err != nil {
						return err
					}
					saved = append(saved, p)
				}
				for _, p := range saved {
					a.printf("Saved %s\n", p)
				}
			} else if r.LogSize > 0 || len(r.Artifacts) > 0 {
				a.printf("Download the log and artifacts: vx check show %s --download DIR\n", r.Id[:8])
			}
			a.emit(map[string]any{"result": r, "saved": saved})
			return nil
		},
	}
	cmd.Flags().StringVar(&download, "download", "", "save the log, artifacts and data into this directory")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func clipText(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
