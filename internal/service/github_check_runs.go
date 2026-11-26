package service

// Checks on GitHub pull requests (#47 part 3): for a shipped effort, every
// check that applies becomes a GitHub check run on the pull request's head
// commit, named "vx: NAME". vx creates it queued (or completed, when the
// result is in) and updates it as results arrive: status, conclusion, the
// summary, who reported it with the trust label, and links to the vx page,
// the log and the artifacts (linked, never uploaded: they stay behind the
// repository's read permission).
//
// It needs the App's Checks permission (read and write). Without it GitHub
// refuses the calls: vx records the gap once (Admin > GitHub then says
// "GitHub checks are off ..."), logs it once, and ships as usual.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// CheckRunPrefix starts the name of every check run vx publishes for a
// check; CI feedback ignores them, like the vx evidence run.
const CheckRunPrefix = "vx: "

// CheckRunName is the GitHub check run name of a check.
func CheckRunName(check string) string { return CheckRunPrefix + check }

// checkRunFor turns one check's place in the pipeline into the check run
// to publish. pub is the server's public URL ("" in tests without one).
func checkRunFor(pub string, repo store.Repo, effort store.Effort, st PipelineCheck, headSHA string) GitHubCheckRun {
	run := GitHubCheckRun{Name: CheckRunName(st.Def.Name), HeadSHA: headSHA, ExternalID: effort.ID.String() + "/" + st.Def.Name}
	if pub != "" {
		run.DetailsURL = pub + "/ui/effort?" + url.Values{"repo": {repo.Name}, "slug": {effort.Slug}, "view": {"checks"}}.Encode() + "#check-" + st.Def.Name
	}
	var lines []string
	from := "From " + st.Origin + "."
	switch st.State {
	case rules.StatePassed, rules.StateFailed, rules.StateNeutral:
		run.Status = "completed"
		run.Conclusion = map[string]string{rules.StatePassed: "success", rules.StateFailed: "failure", rules.StateNeutral: "neutral"}[st.State]
		run.Title = map[string]string{rules.StatePassed: "Passed", rules.StateFailed: "Failed", rules.StateNeutral: "Neutral"}[st.State]
		if r := st.Result; r != nil {
			if r.Summary != "" {
				run.Title += ": " + clipRunes(firstLine(r.Summary), 120)
				lines = append(lines, r.Summary, "")
			}
			lines = append(lines, "Reported by "+r.Reporter+" ("+st.Trust+")"+onBehalf(r)+".")
			if pub != "" {
				q := url.Values{"repo": {repo.Name}, "result": {r.ID.String()}}
				if !r.LogHash.IsZero() {
					lines = append(lines, fmt.Sprintf("Log: [%s](%s/ui/checks/log?%s)", "log", pub, q.Encode()))
				}
				for _, a := range r.Artifacts {
					aq := url.Values{"repo": {repo.Name}, "result": {r.ID.String()}, "name": {a.Name}}
					lines = append(lines, fmt.Sprintf("- [%s](%s/ui/checks/artifact?%s) (%s)", mdEscape(a.Name), pub, aq.Encode(), humanSize(a.Size)))
				}
			} else if len(r.Artifacts) > 0 {
				lines = append(lines, fmt.Sprintf("%d artifact(s) on vx.", len(r.Artifacts)))
			}
		}
	case rules.StateSkipped:
		run.Status, run.Conclusion = "completed", "skipped"
		run.Title = "Skipped: " + strings.TrimPrefix(st.Reason, "skipped: ")
		lines = append(lines, "This check runs after others on the same tree; "+strings.TrimPrefix(st.Reason, "skipped: ")+", so it will not run on this commit.")
	case rules.StateWaiting:
		run.Status = "queued"
		run.Title = "Waits for " + strings.TrimPrefix(st.Reason, "waits for ")
		lines = append(lines, "It runs once those checks passed on this tree.")
	default:
		run.Status = "queued"
		run.Title = "Waiting for a result"
		if st.Rejected != "" {
			lines = append(lines, "A result was reported but does not count: "+st.Rejected+".")
		}
		lines = append(lines, "Reported with vx check report "+st.Def.Name+".")
	}
	run.Summary = clipRunes(strings.TrimSpace(strings.Join(append(lines, "", from), "\n")), 60000)
	return run
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func onBehalf(r *CheckResult) string {
	if r.OnBehalfOf != "" && r.OnBehalfOf != r.Reporter {
		return " for " + r.OnBehalfOf
	}
	return ""
}

// mdEscape keeps a file name from turning into markdown.
func mdEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "*", `\*`, "_", `\_`, "`", "\\`", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// fingerprint says whether a run changed since it was last published.
func (r GitHubCheckRun) fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join([]string{r.Status, r.Conclusion, r.Title, r.Summary, r.DetailsURL}, "\x00")))
	return hex.EncodeToString(h[:8])
}

// publishCheckRuns brings the check runs on the heads of an effort's open
// GitHub pull requests up to date. Errors are logged, never returned:
// results and ships go on without GitHub.
func (s *Service) publishCheckRuns(ctx context.Context, repo store.Repo, effort store.Effort) {
	if repo.Provider != store.ProviderGitHub || repo.GitHubInstallationID == nil {
		return
	}
	owner, name, ok := strings.Cut(repo.Remote, "/")
	if !ok {
		return
	}
	shipments, err := s.Store.ListShipments(ctx, effort.ID)
	if err != nil || len(shipments) == 0 {
		return
	}
	api, err := s.GitHubClient(ctx)
	if err != nil {
		return
	}
	runs, ok := api.(GitHubCheckRunsAPI)
	if !ok {
		return
	}
	pub := s.publicURL()
	// The checks of a shipped effort are those on its head (what the ship
	// gate reads); every pull request of a split shows them.
	head, err := s.Store.GetChangeset(ctx, repo.ID, effort.Head)
	if err != nil {
		slog.Default().Warn("check runs: effort head", "repo", repo.Name, "effort", effort.Slug, "err", err)
		return
	}
	checks, err := s.pipelineChecks(ctx, repo, effort, "", head.Tree)
	if err != nil {
		slog.Default().Warn("check runs: checks", "repo", repo.Name, "effort", effort.Slug, "err", err)
		return
	}
	sent := 0
	for _, sh := range shipments {
		if (sh.State != "open" && sh.State != "draft") || sh.HeadGitSHA == "" || sh.ProviderRef == "" {
			continue
		}
		published, err := s.Store.GitHubCheckRuns(ctx, sh.ID, sh.HeadGitSHA)
		if err != nil {
			continue
		}
		have := map[string]store.GitHubCheckRunRow{}
		for _, p := range published {
			have[p.CheckName] = p
		}
		for _, st := range checks {
			if st.State == rules.StateNotHere {
				continue
			}
			run := checkRunFor(pub, repo, effort, st, sh.HeadGitSHA)
			fp := run.fingerprint()
			prev, had := have[st.Def.Name]
			if had && prev.State == fp {
				continue
			}
			id := prev.GitHubID
			if had {
				err = runs.UpdateCheckRun(ctx, *repo.GitHubInstallationID, owner, name, id, run)
			} else {
				id, err = runs.StartCheckRun(ctx, *repo.GitHubInstallationID, owner, name, run)
			}
			if errors.Is(err, ErrGitHubForbidden) {
				s.checksForbidden(ctx, repo)
				return
			}
			if err != nil {
				slog.Default().Warn("check run", "repo", repo.Name, "check", st.Def.Name, "pr", sh.URL, "err", err)
				continue
			}
			sent++
			if err := s.Store.SaveGitHubCheckRun(ctx, store.GitHubCheckRunRow{ShipmentID: sh.ID, CheckName: st.Def.Name, HeadSHA: sh.HeadGitSHA, GitHubID: id, State: fp}); err != nil {
				slog.Default().Warn("record check run", "repo", repo.Name, "check", st.Def.Name, "err", err)
			}
		}
	}
	if sent > 0 {
		_ = s.Store.ClearGitHubAppGap(ctx, gapChecksWrite)
	}
}

// checksForbidden records that GitHub refused a check run for want of the
// Checks permission, and logs it the first time.
func (s *Service) checksForbidden(ctx context.Context, repo store.Repo) {
	gaps, _ := s.Store.GitHubAppGaps(ctx)
	known := false
	for _, g := range gaps {
		if g.What == gapChecksWrite {
			known = true
		}
	}
	if !known {
		slog.Default().Warn(GitHubChecksOffNotice+" (Checks: Read and write); vx publishes no check runs until it has it", "repo", repo.Name)
	}
	if err := s.Store.AddGitHubAppGap(ctx, gapChecksWrite, "GitHub refused the vx check runs on "+repo.Remote); err != nil {
		slog.Default().Warn("record GitHub App gap", "err", err)
	}
}

// PipelineCheck is one check of a change or effort on one tree, placed in
// its pipeline: what the status, the web pages and GitHub show.
type PipelineCheck struct {
	Def    rules.CheckDef
	Origin string // "organization acme (version 3)" or "vx.toml"
	Depth  int    // for indented display: 0 runs after nothing
	Parent string // the check it is shown under
	State  string // rules.StatePassed, ... (see rules.Pipeline)
	Reason string // why it waits or was skipped
	Result *CheckResult
	// Trust labels the reporter of Result: "independent agent", "CI",
	// "person", "the author's agent", ...
	Trust string
	// Rejected says why the newest result does not count (the reporter is
	// not one the check accepts); empty otherwise.
	Rejected string
}

// pipelineChecks places the checks of a change (or, with changeID empty,
// of the effort head) on tree in their pipeline, without a permission
// check: callers have authorized the repository.
func (s *Service) pipelineChecks(ctx context.Context, repo store.Repo, effort store.Effort, changeID string, tree hash.Hash) ([]PipelineCheck, error) {
	view, err := s.checkStatusOf(ctx, repo, effort, changeID, tree)
	if err != nil {
		return nil, err
	}
	org, _ := s.orgChecksOf(ctx, repo)
	return placeChecks(view, org), nil
}

// placeChecks runs rules.Pipeline over a status view.
func placeChecks(view CheckStatusView, org OrgChecks) []PipelineCheck {
	defs := make([]rules.CheckDef, 0, len(view.Checks))
	results := map[string]string{}
	byName := map[string]CheckState{}
	notHere := map[string]bool{}
	for _, c := range view.Checks {
		defs = append(defs, c.Def)
		byName[c.Def.Name] = c
		switch c.State {
		case "passed":
			results[c.Def.Name] = rules.ResultPass
		case "failed":
			results[c.Def.Name] = rules.ResultFail
		case "neutral":
			results[c.Def.Name] = rules.ResultNeutral
		case CheckStateNotNeeded:
			notHere[c.Def.Name] = true
		}
	}
	var out []PipelineCheck
	for _, st := range rules.Pipeline(rules.StepsOf(defs), results, func(n string) bool { return !notHere[n] }) {
		c := byName[st.Name]
		pc := PipelineCheck{Def: c.Def, Origin: CheckOrigin(c.Def, org), Depth: st.Depth, Parent: st.Parent, State: st.State, Reason: st.Reason, Result: c.Latest}
		if c.Latest != nil {
			pc.Trust = TrustLabel(c.Latest)
		}
		if nc := c.NotCounted(); len(nc) > 0 {
			pc.Rejected = nc[0].Reporter + ": " + nc[0].Why
		}
		if st.State == rules.StateReady && c.State == CheckStatePending {
			pc.Reason = "pending: a result is being made"
		}
		out = append(out, pc)
	}
	return out
}

// TrustLabel names who reported a result, for people: "CI", "person",
// "agent, accepted" or "agent, not accepted" (by the check's reporter
// rule).
func TrustLabel(r *CheckResult) string {
	if r == nil {
		return ""
	}
	switch {
	case r.CITokenID != nil:
		return "CI"
	case strings.HasPrefix(r.Reporter, "user:"):
		return "person"
	case r.Counts:
		return "agent, accepted"
	}
	return "agent, not accepted"
}
