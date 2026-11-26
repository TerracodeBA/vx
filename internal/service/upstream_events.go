package service

// Host-neutral handling of what a git host tells vx: a push to a branch and
// a merged vx pull request. GitHub's webhook (github_events.go) and the
// GitLab, Gitea, Forgejo and Bitbucket webhooks (host_webhooks.go) parse
// their own payloads, find the repository and call these.

import (
	"context"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// applyUpstreamPush imports a push to branch of repo when vx follows the
// branch (the default branch, or one an effort started from), like
// `vx repo sync`, then marks merged efforts and refreshes the overlap
// forecast. by names the source in the op log ("github push").
func (s *Service) applyUpstreamPush(ctx context.Context, repo store.Repo, branch, by string) error {
	if s.Upstream == nil || branch == "" {
		return nil
	}
	if branch != repo.DefaultBranch {
		if _, err := s.Store.GetBranch(ctx, repo.ID, branch); err != nil {
			return nil // a branch vx does not follow
		}
	}
	if !s.agreementOK(ctx, repo.OrgID) {
		return nil // imported on the first sync after the agreement is accepted (dpa.go)
	}
	b, n, err := s.Upstream.SyncBranch(ctx, repo, branch)
	if err != nil {
		return errf(CodeUnavailable, "import the push to %s of %s: %v", branch, repo.Name, err)
	}
	if n > 0 {
		_ = s.Store.LogOp(ctx, &repo.ID, nil, "branch.sync", branch, nil, map[string]any{"head": b.Head.String(), "git_sha": b.GitSHA, "imported": n, "by": by})
		s.scheduleBranchForecast(repo, branch)
	}
	// Also when nothing new came in: a merge vx missed may be on it.
	s.BranchMoved(ctx, repo, branch, by)
	return nil
}

// applyMergedPullRequest handles a merged pull request from head into base
// on repo: it imports base now (the push may arrive later, or not at all
// for squash merges) and marks efforts whose changes are on it merged.
// With a squash merge the commits lose vx's change-id trailers, so the
// shipment pushed to the vx/ head branch is marked merged directly, and its
// effort once no other part of a split is open. host names
// the git host for the effort's history ("GitHub", "GitLab"). Heads
// outside vx/ are ignored.
func (s *Service) applyMergedPullRequest(ctx context.Context, repo store.Repo, head, base, host string) error {
	if slug, ok := strings.CutPrefix(head, "vx/"); !ok || slug == "" {
		return nil
	}
	if base == "" {
		base = repo.DefaultBranch
	}
	// The other efforts on base rebase once this one is closed.
	defer s.autoRebase(context.WithoutCancel(ctx), repo, base, strings.ToLower(host)+" pull request merged")
	if s.Upstream != nil {
		if b, n, err := s.Upstream.SyncBranch(ctx, repo, base); err == nil && n > 0 {
			_ = s.Store.LogOp(ctx, &repo.ID, nil, "branch.sync", base, nil, map[string]any{"head": b.Head.String(), "git_sha": b.GitSHA, "imported": n, "by": strings.ToLower(host) + " pull request merged"})
			s.scheduleBranchForecast(repo, base)
		}
	}
	s.markMergedEfforts(ctx, repo, base)
	_, err := s.markShipmentMergedByHead(ctx, repo, head, "its pull request was merged on "+host)
	return err
}
