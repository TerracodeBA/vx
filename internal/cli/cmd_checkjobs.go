package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// ---------- check queue: vx check next|renew|release|jobs|job|request ----------

// checkCmd is vx check: company-defined checks (#47). The queue's
// subcommands are below; results and status are added next to them.
// checkQueueCmds are the queue's subcommands of vx check (#47).
func (a *App) checkQueueCmds() []*cobra.Command {
	return []*cobra.Command{a.checkNextCmd(), a.checkRenewCmd(), a.checkReleaseCmd(), a.checkJobsCmd(), a.checkJobCmd(), a.checkRequestCmd()}
}

// softRepo is the repository from a flag, VX_REPO or the working copy, or
// "" when there is none (every repository).
func (a *App) softRepo(flag string) string {
	if r, err := a.resolveRepo(flag); err == nil {
		return r
	}
	return ""
}

func (a *App) checkNextCmd() *cobra.Command {
	var repo, effort, runner string
	var wait bool
	var timeout, interval time.Duration
	cmd := &cobra.Command{
		Use:   "next [--runner KIND] [--wait [--timeout 10m]]",
		Short: "Take the oldest check job you may run, with a lease",
		Long: `Take the oldest queued check job you may run and hold it with a lease for
the check's timeout. Report its result with vx check report (that finishes
the job); renew the lease with vx check renew while you work, or give the job
back with vx check release. A lease that expires puts the job back in the
queue with the attempt counted; after 3 it fails.

Who gets what: agent:KIND jobs go to agents whose person declared KIND for
them (vx agent runner NAME --kinds KIND), ci jobs to CI tokens of the
repository, human jobs to people. A job whose result you could not report
(an independent check of your own change) is never given to you.

Without --repo (or VX_REPO, or a working copy) it takes jobs from every
repository you may run jobs in. With --wait it waits until there is one
(exit 4 after --timeout).`,
		Example: `  vx check next --runner browser --wait
  vx check next --runner ci --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			req := &vxv1.NextCheckJobRequest{Repo: a.softRepo(repo), Effort: effort, Runner: runner}
			if effort != "" && req.Repo == "" {
				return errors.New("--effort needs --repo (or VX_REPO, or a working copy)")
			}
			var deadline time.Time
			if timeout > 0 {
				deadline = time.Now().Add(timeout)
			}
			for {
				resp, err := c.Queue.NextCheckJob(ctxOf(cmd), connect.NewRequest(req))
				if err != nil {
					return queueUnsupported(err)
				}
				if j := resp.Msg.Job; j != nil {
					a.printJob(j, true)
					a.emit(resp.Msg)
					return nil
				}
				if !wait {
					a.printf("No check job for you now. Wait for one: vx check next --wait\n")
					a.emit(resp.Msg)
					return nil
				}
				if !deadline.IsZero() && time.Now().After(deadline) {
					a.emit(resp.Msg)
					return &exitError{code: ExitTimeout, err: fmt.Errorf("no check job within %s", timeout)}
				}
				select {
				case <-ctxOf(cmd).Done():
					return ctxOf(cmd).Err()
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository (default: VX_REPO or the working copy's; none: every repository)")
	cmd.Flags().StringVar(&effort, "effort", "", "only this effort")
	cmd.Flags().StringVar(&runner, "runner", "", "only this runner kind you serve: browser (agent:browser), ci or human")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until there is a job")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "with --wait: give up after this long (exit 4); 0: wait forever")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "with --wait: how often to ask")
	return cmd
}

// printJob prints a job; full adds its inputs and what to run next.
func (a *App) printJob(j *vxv1.CheckJob, full bool) {
	id := noteID(j.Id)
	a.printf("Job %s: %s on %s/%s (%s, %s)\n", id, j.Check, j.Repo, j.Effort, j.Runner, j.State)
	if j.ChangeId != "" {
		a.printf("  change   %s by %s, tree %s\n", j.ChangeId, j.Author, short(j.Tree))
	} else {
		a.printf("  tree     %s\n", short(j.Tree))
	}
	switch j.State {
	case "leased":
		a.printf("  lease    %s until %s\n", j.LeaseOwner, j.LeaseExpiresAt)
	case "failed", "superseded":
		a.printf("  why      %s\n", j.Reason)
	case "done":
		a.printf("  result   %s\n", j.ResultId)
	case "queued":
		if j.Attempts > 0 || j.Reason != "" {
			a.printf("  note     %s\n", j.Reason)
		}
	}
	if !full || j.Bundle == nil {
		return
	}
	b := j.Bundle
	if b.Change.GetIntent() != "" {
		a.printf("  intent   %s\n", firstLine(b.Change.GetIntent()))
	}
	keys := make([]string, 0, len(b.Values))
	for k := range b.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a.printf("  input    %s = %s\n", k, b.Values[k])
	}
	for _, m := range b.Missing {
		a.printf("  input    %s: not given (ask the requester: vx check request %s --value %s=...)\n", m, j.Check, m)
	}
	a.printf("  files    %d changed", len(b.ChangedFiles))
	if len(b.ChangedFiles) > 0 {
		var ps []string
		for i, f := range b.ChangedFiles {
			if i == 5 {
				ps = append(ps, "...")
				break
			}
			ps = append(ps, f.Path)
		}
		a.printf(": %s", strings.Join(ps, ", "))
	}
	a.printf("\n")
	if b.Diff != "" {
		a.printf("  diff     %d lines (vx check job %s --json has it)\n", strings.Count(b.Diff, "\n"), id)
	}
	if j.State == "leased" {
		a.printf("\nReport:  vx check report %s --repo %s --effort %s", j.Check, j.Repo, j.Effort)
		if j.ChangeId != "" {
			a.printf(" --change %s", j.ChangeId)
		}
		a.printf(" --tree %s --status pass|fail --summary \"...\"\n", j.Tree)
		a.printf("Renew:   vx check renew %s    Give back: vx check release %s --reason \"...\"\n", id, id)
	}
}

func (a *App) checkRenewCmd() *cobra.Command {
	var repo string
	var d time.Duration
	cmd := &cobra.Command{
		Use:   "renew JOB [--for 30m]",
		Short: "Extend your lease on a check job",
		Long: `Extend your lease on a check job you took with vx check next, from now by
--for (default: the check's timeout). A job that was superseded by a newer
tree, or whose lease expired and went to someone else, says so: stop and
take the next one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.RenewCheckJob(ctxOf(cmd), connect.NewRequest(&vxv1.RenewCheckJobRequest{Repo: a.softRepo(repo), Id: args[0], Seconds: int64(d / time.Second)}))
			if err != nil {
				return queueUnsupported(err)
			}
			j := resp.Msg.Job
			a.printf("Renewed job %s (%s): leased until %s.\n", noteID(j.Id), j.Check, j.LeaseExpiresAt)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "the job's repository (default: VX_REPO or the working copy's; none: any)")
	cmd.Flags().DurationVar(&d, "for", 0, "new lease, from now (default: the check's timeout)")
	return cmd
}

func (a *App) checkReleaseCmd() *cobra.Command {
	var repo, reason string
	cmd := &cobra.Command{
		Use:   "release JOB [--reason TEXT]",
		Short: "Give a check job back to the queue without a result",
		Long: `Give a check job you hold back to the queue, without counting an attempt,
for example when this machine cannot run it. The reason is shown with the
job.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.ReleaseCheckJob(ctxOf(cmd), connect.NewRequest(&vxv1.ReleaseCheckJobRequest{Repo: a.softRepo(repo), Id: args[0], Reason: reason}))
			if err != nil {
				return queueUnsupported(err)
			}
			j := resp.Msg.Job
			a.printf("Released job %s (%s): it is queued again.\n", noteID(j.Id), j.Check)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "the job's repository (default: VX_REPO or the working copy's; none: any)")
	cmd.Flags().StringVar(&reason, "reason", "", "why, for whoever takes it next")
	return cmd
}

func (a *App) checkJobsCmd() *cobra.Command {
	var repo, effort, runner string
	var states []string
	var all bool
	cmd := &cobra.Command{
		Use:   "jobs [--effort SLUG] [--state queued]... [--runner KIND] [--all]",
		Short: "List the check queue of a repository or effort",
		Long: `List check jobs, waiting ones first: what each check waits for, who holds
it and until when, and why jobs failed or were superseded. In a working copy
it lists the effort's jobs; --all lists the whole repository's.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			req := &vxv1.ListCheckJobsRequest{Repo: a.softRepo(repo), Effort: effort, States: states, Runner: runner}
			if req.Effort == "" && !all && repo == "" {
				if wc, err := a.workingCopy(); err == nil && wc.Config.Repo == req.Repo {
					req.Effort = wc.Config.Effort
				}
			}
			if req.Effort != "" && req.Repo == "" {
				return errors.New("--effort needs --repo (or VX_REPO, or a working copy)")
			}
			resp, err := c.Queue.ListCheckJobs(ctxOf(cmd), connect.NewRequest(req))
			if err != nil {
				return queueUnsupported(err)
			}
			if len(resp.Msg.Jobs) == 0 {
				a.printf("No check jobs.\n")
			}
			for _, j := range resp.Msg.Jobs {
				a.printJob(j, false)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository (default: VX_REPO or the working copy's; none: every repository you can read)")
	cmd.Flags().StringVar(&effort, "effort", "", "only this effort (default in a working copy: its effort)")
	cmd.Flags().StringSliceVar(&states, "state", nil, "queued, leased, done, failed or superseded (default: all)")
	cmd.Flags().StringVar(&runner, "runner", "", "only jobs for this runner: browser (agent:browser), ci or human")
	cmd.Flags().BoolVar(&all, "all", false, "the whole repository, not only the working copy's effort")
	return cmd
}

func (a *App) checkJobCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "job JOB",
		Short: "Show one check job with its inputs (diff, files, values)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.GetCheckJob(ctxOf(cmd), connect.NewRequest(&vxv1.GetCheckJobRequest{Repo: a.softRepo(repo), Id: args[0]}))
			if err != nil {
				return queueUnsupported(err)
			}
			a.printJob(resp.Msg.Job, true)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "the job's repository (default: VX_REPO or the working copy's; none: any)")
	return cmd
}

func (a *App) checkRequestCmd() *cobra.Command {
	var repo, effortFlag, change string
	var values []string
	cmd := &cobra.Command{
		Use:   "request NAME [--change ID] [--value NAME=VALUE]...",
		Short: "Ask for a check with a runner to run now (it becomes a job)",
		Long: `Make a job for a check of vx.toml whose runner is an agent kind, ci or
human: on the effort's head (its whole diff from the base), or on one change
with --change. Any check with a runner can be requested, also "manual" ones
and to run a land or ship check again. --value gives named inputs the check
declares, such as a preview URL; asking again for the same tree adds them to
the waiting job.`,
		Example: `  vx check request browser-e2e --value preview_url=https://pr-12.preview.example.com
  vx check request security-review --change 8tm9-f2kq`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var slug []string
			if effortFlag != "" {
				slug = []string{effortFlag}
			}
			repo, effort, err := a.resolveEffort(repo, slug)
			if err != nil {
				return err
			}
			vals := map[string]string{}
			for _, kv := range values {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || strings.TrimSpace(k) == "" {
					return fmt.Errorf("--value %q: give NAME=VALUE, like preview_url=https://...", kv)
				}
				vals[strings.TrimSpace(k)] = v
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.RequestCheck(ctxOf(cmd), connect.NewRequest(&vxv1.RequestCheckRequest{Repo: repo, Effort: effort, Check: args[0], ChangeId: change, Values: vals}))
			if err != nil {
				return queueUnsupported(err)
			}
			j := resp.Msg.Job
			a.printf("Requested %s: job %s waits for a %s runner (vx check jobs shows the queue).\n", j.Check, noteID(j.Id), j.Runner)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repository (default: VX_REPO or the working copy's)")
	cmd.Flags().StringVar(&effortFlag, "effort", "", "effort slug (default: the working copy's)")
	cmd.Flags().StringVar(&change, "change", "", "run it on this change (default: the effort head)")
	cmd.Flags().StringArrayVar(&values, "value", nil, "a named input, NAME=VALUE (repeatable)")
	return cmd
}

// queueUnsupported explains an old server.
func queueUnsupported(err error) error {
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return errors.New("this vx server has no check queue yet: ask its operator to update it")
	}
	return err
}

// ---------- vx agent runner ----------

func (a *App) agentRunnerCmd() *cobra.Command {
	var kinds []string
	cmd := &cobra.Command{
		Use:   "runner NAME [--kinds browser,security]",
		Short: "Say which kinds of check jobs one of your agents runs",
		Long: `Say which runner kinds one of your agents serves: a check with
runner = "agent:browser" becomes a job only agents that serve browser take
(vx check next). --kinds replaces the list; --kinds '' empties it. Without
--kinds it shows the list.

Only you (the agent's person) can change it; the agent itself cannot. A
runner runs untrusted change content: see docs/runners.md.`,
		Example: `  vx agent runner qa --kinds browser
  vx agent runner qa --kinds ''`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var agent *vxv1.Principal
			var got []string
			if cmd.Flags().Changed("kinds") {
				resp, err := c.Queue.SetAgentRunner(ctxOf(cmd), connect.NewRequest(&vxv1.SetAgentRunnerRequest{Agent: args[0], Kinds: kinds}))
				if err != nil {
					return queueUnsupported(err)
				}
				agent, got = resp.Msg.Agent, resp.Msg.Kinds
				a.emit(resp.Msg)
			} else {
				resp, err := c.Queue.GetAgentRunner(ctxOf(cmd), connect.NewRequest(&vxv1.GetAgentRunnerRequest{Agent: args[0]}))
				if err != nil {
					return queueUnsupported(err)
				}
				agent, got = resp.Msg.Agent, resp.Msg.Kinds
				a.emit(resp.Msg)
			}
			if len(got) == 0 {
				a.printf("%s runs no check jobs. Give it some: vx agent runner %s --kinds browser\n", agent.Ref, args[0])
				return nil
			}
			a.printf("%s runs check jobs of kind %s (runner = \"agent:%s\").\n", agent.Ref, strings.Join(got, ", "), got[0])
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&kinds, "kinds", nil, "runner kinds, comma separated: browser,security")
	return cmd
}

// ---------- vx repo ci-token ----------

func (a *App) repoCITokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci-token",
		Short: "Create, list and revoke CI tokens: they only report check results",
		Long: `A CI token lets a CI system report check results on one repository (for
the checks you name, or every check) and read its check jobs and their
files. It can do nothing else: to it every other operation, on this
repository or any other, is "not found". Repository admins manage them.

  vx repo ci-token create github-actions --repo acme/shop --checks unit,lint
  vx repo ci-token list --repo acme/shop
  vx repo ci-token revoke github-actions --repo acme/shop

docs/runners.md has a GitHub Actions example.`,
	}
	var repo string
	var checks []string
	create := &cobra.Command{
		Use:   "create NAME [--checks unit,lint]",
		Short: "Create a CI token; its secret is shown once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.resolveRepo(repo)
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.CreateCIToken(ctxOf(cmd), connect.NewRequest(&vxv1.CreateCITokenRequest{Repo: r, Name: args[0], Checks: checks}))
			if err != nil {
				return queueUnsupported(err)
			}
			t := resp.Msg.Token
			what := "every check"
			if len(t.Checks) > 0 {
				what = strings.Join(t.Checks, ", ")
			}
			a.printf("Created CI token %s on %s (%s); it may report %s.\n\nToken (shown once; store it as a CI secret, e.g. VX_TOKEN):\n  %s\n",
				t.Name, t.Repo, t.Principal, what, resp.Msg.Secret)
			a.emit(resp.Msg)
			return nil
		},
	}
	create.Flags().StringVar(&repo, "repo", "", "repository (default: VX_REPO or the working copy's)")
	create.Flags().StringSliceVar(&checks, "checks", nil, "the checks it may report, comma separated (default: every check)")
	var listRepo string
	var revoked bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List a repository's CI tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := a.resolveRepo(listRepo)
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.ListCITokens(ctxOf(cmd), connect.NewRequest(&vxv1.ListCITokensRequest{Repo: r, Revoked: revoked}))
			if err != nil {
				return queueUnsupported(err)
			}
			if len(resp.Msg.Tokens) == 0 {
				a.printf("%s has no CI tokens. Create one: vx repo ci-token create NAME --repo %s\n", r, r)
			}
			for _, t := range resp.Msg.Tokens {
				what := "every check"
				if len(t.Checks) > 0 {
					what = strings.Join(t.Checks, ", ")
				}
				state := "active"
				if t.RevokedAt != "" {
					state = "revoked " + t.RevokedAt
				}
				a.printf("%-20s %-22s %s; created by %s %s; %s\n", t.Name, t.Principal, what, t.CreatedBy, t.CreatedAt, state)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	list.Flags().StringVar(&listRepo, "repo", "", "repository (default: VX_REPO or the working copy's)")
	list.Flags().BoolVar(&revoked, "revoked", false, "include revoked tokens")
	var revokeRepo string
	revoke := &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke a CI token at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.resolveRepo(revokeRepo)
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Queue.RevokeCIToken(ctxOf(cmd), connect.NewRequest(&vxv1.RevokeCITokenRequest{Repo: r, Name: args[0]}))
			if err != nil {
				return queueUnsupported(err)
			}
			a.printf("Revoked CI token %s on %s.\n", resp.Msg.Token.Name, resp.Msg.Token.Repo)
			a.emit(resp.Msg)
			return nil
		},
	}
	revoke.Flags().StringVar(&revokeRepo, "repo", "", "repository (default: VX_REPO or the working copy's)")
	cmd.AddCommand(create, list, revoke)
	return cmd
}
