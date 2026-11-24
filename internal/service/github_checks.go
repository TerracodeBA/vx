package service

// The "vx evidence" check run vx posts on the head of its GitHub pull
// requests: the checks vx ran, those reported by agents, those from before
// a merge or rebase, the required checks nobody verified, and warnings.
// It needs the App permission checks: write. An App created before v0.4
// lacks it: the first refusal is recorded and shown on Admin > GitHub, and
// shipping goes on without the check run.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// GitHubChecksAPI is the part of the GitHub client that posts check runs
// and reads the App's settings. internal/github implements it next to
// GitHubAPI; a client without it posts no check runs.
type GitHubChecksAPI interface {
	CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run GitHubCheckRun) error
	// AppSettings reads the App's permissions and webhook events.
	AppSettings(ctx context.Context) (GitHubAppSettings, error)
}

// GitHubCheckRun is a completed check run on a commit.
type GitHubCheckRun struct {
	Name       string
	HeadSHA    string
	Conclusion string // success, neutral, failure
	Title      string
	Summary    string // markdown
	Text       string // markdown
	DetailsURL string
	ExternalID string
}

// GitHubAppSettings are the App's permissions and events on GitHub.
type GitHubAppSettings struct {
	Permissions map[string]string
	Events      []string
}

// ErrGitHubForbidden matches (errors.Is) a call GitHub refused because the
// App lacks a permission (403, not a rate limit).
var ErrGitHubForbidden = errors.New("GitHub refused the call: the App lacks a permission")

// Gaps vx records when GitHub refuses a call.
const gapChecksWrite = "checks:write"

// postEvidenceCheck posts the vx evidence check run on a shipped pull
// request's head. Failures are logged, never returned: the pull request
// is open either way.
func (s *Service) postEvidenceCheck(ctx context.Context, plan ShipPlan, headSHA, prURL string) {
	repo := plan.Repo
	if repo.Provider != store.ProviderGitHub || repo.GitHubInstallationID == nil || headSHA == "" {
		return
	}
	owner, name, ok := strings.Cut(repo.Remote, "/")
	if !ok {
		return
	}
	api, err := s.GitHubClient(ctx)
	if err != nil {
		return
	}
	checks, ok := api.(GitHubChecksAPI)
	if !ok {
		return
	}
	run := evidenceCheckRun(plan, headSHA)
	if pub := s.publicURL(); pub != "" {
		run.DetailsURL = pub + "/ui/ship?" + url.Values{"repo": {repo.Name}, "slug": {plan.Effort.Slug}}.Encode()
	}
	err = checks.CreateCheckRun(ctx, *repo.GitHubInstallationID, owner, name, run)
	switch {
	case errors.Is(err, ErrGitHubForbidden):
		if err := s.Store.AddGitHubAppGap(ctx, gapChecksWrite, "GitHub refused the vx evidence check run on "+repo.Remote); err != nil {
			slog.Default().Warn("record GitHub App gap", "err", err)
		}
	case err != nil:
		slog.Default().Warn("vx evidence check run", "repo", repo.Name, "pr", prURL, "err", err)
	default:
		_ = s.Store.ClearGitHubAppGap(ctx, gapChecksWrite)
	}
}

// evidenceCheckRun sums up the evidence of a ship plan's commits.
func evidenceCheckRun(p ShipPlan, headSHA string) GitHubCheckRun {
	var ran, reported, stale, failed, notVerified int
	var lines []string
	for _, st := range p.Commits() {
		c := st.Changeset
		lines = append(lines, fmt.Sprintf("### %s (`%s`, %s)", c.Title(), c.ChangeID, c.Author.Principal))
		if c.Evidence == nil || (len(c.Evidence.Checks) == 0 && len(c.Evidence.NotVerified) == 0) {
			lines = append(lines, "- no evidence")
			continue
		}
		for _, ch := range c.Evidence.Checks {
			text := checkText(ch)
			switch {
			case ch.Result != "pass":
				failed++
			case ch.RanByVX:
				ran++
			default:
				reported++
			}
			if c.Merged() {
				text += "; before merge: on the author's snapshot, before vx merged it onto newer work"
				stale++
			} else if c.Rebased() {
				text += "; before rebase onto " + rebasedOnto(c.Changeset)
				stale++
			}
			lines = append(lines, "- `"+ch.Name+"`: "+text)
		}
		for _, nv := range c.Evidence.NotVerified {
			notVerified++
			lines = append(lines, "- not verified: "+rules.NotVerified(nv))
		}
	}
	if len(p.Warnings) > 0 {
		lines = append(lines, "### Warnings")
		for _, w := range p.Warnings {
			lines = append(lines, "- "+w.Detail)
		}
	}
	conclusion := "success"
	if reported > 0 || stale > 0 || notVerified > 0 || len(p.Warnings) > 0 {
		conclusion = "neutral"
	}
	if failed > 0 {
		conclusion = "failure"
	}
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(ran, "run by vx")
	add(reported, "reported")
	add(stale, "before merge/rebase")
	add(failed, "failed")
	add(notVerified, "required, not verified")
	add(len(p.Warnings), "warning(s)")
	title := "No evidence"
	if len(parts) > 0 {
		title = "Checks: " + strings.Join(parts, ", ")
	}
	summary := "What the agents of vx effort `" + p.Effort.Slug + "` ran before shipping. Checks \"run by vx\" were run by the vx CLI and their result recorded by it; \"reported\" ones are an agent's word. This is vx's record, not your CI."
	return GitHubCheckRun{
		Name: EvidenceCheckName, HeadSHA: headSHA, Conclusion: conclusion, Title: title,
		Summary: summary, Text: clipRunes(strings.Join(lines, "\n"), 60000), ExternalID: p.Effort.ID.String(),
	}
}

// GitHubAppGap is something the server's App lacks, for Admin > GitHub.
type GitHubAppGap struct {
	What string // e.g. "checks:write" or "event:check_run"
	Fix  string // what to do on GitHub
}

// GitHubAppGaps lists the permissions and events this vx needs that the
// App lacks (superadmins): those GitHub refused, and those the App's
// settings on GitHub do not have.
func (s *Service) GitHubAppGaps(ctx context.Context, actor Actor) ([]GitHubAppGap, error) {
	if !s.IsSuperadmin(ctx, actor) {
		return nil, errf(CodePermission, "only superadmins can see the GitHub App's settings")
	}
	app, err := s.GitHubApp(ctx)
	if err != nil {
		return nil, err
	}
	settings := "the App's settings on GitHub (" + strings.TrimRight(app.HTMLURL, "/") + ", Edit, Permissions & events)"
	seen := map[string]bool{}
	var out []GitHubAppGap
	addPerm := func(perm, level string) {
		key := perm + ":" + level
		if seen[key] {
			return
		}
		seen[key] = true
		label := strings.ToUpper(perm[:1]) + strings.ReplaceAll(perm[1:], "_", " ")
		out = append(out, GitHubAppGap{What: key, Fix: fmt.Sprintf("Grant %s: %s in %s, then accept the new permission on each installation (GitHub asks the organization owners). Until then vx skips what needs it.", label, titleLevel(level), settings)})
	}
	gaps, err := s.Store.GitHubAppGaps(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range gaps {
		if perm, level, ok := strings.Cut(g.What, ":"); ok {
			addPerm(perm, level)
		}
	}
	api, err := s.GitHubClient(ctx)
	if err != nil {
		return out, nil
	}
	checks, ok := api.(GitHubChecksAPI)
	if !ok {
		return out, nil
	}
	have, err := checks.AppSettings(ctx)
	if err != nil {
		return out, nil // GitHub unreachable: the recorded gaps are all we know
	}
	for _, perm := range sortedKeys(boolKeys(GitHubAppPermissions)) {
		if !levelCovers(have.Permissions[perm], GitHubAppPermissions[perm]) {
			addPerm(perm, GitHubAppPermissions[perm])
		}
	}
	events := map[string]bool{}
	for _, e := range have.Events {
		events[e] = true
	}
	var missing []string
	for _, e := range GitHubAppEvents {
		if !events[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		out = append(out, GitHubAppGap{What: "events", Fix: fmt.Sprintf("Subscribe to the events %s in %s.", strings.Join(missing, ", "), settings)})
	}
	return out, nil
}

func titleLevel(level string) string {
	if level == "" {
		return level
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

// levelCovers reports whether permission level have includes want.
func levelCovers(have, want string) bool {
	rank := map[string]int{"": 0, "read": 1, "write": 2, "admin": 3}
	return rank[have] >= rank[want]
}

func boolKeys(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
