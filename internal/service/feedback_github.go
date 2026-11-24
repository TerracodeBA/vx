package service

// GitHub's review and CI webhooks, parsed into the host-neutral ReviewEvent
// and CIEvent (feedback.go): pull_request_review, pull_request_review_comment,
// issue_comment on pull requests, check_run, check_suite and status.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// githubFeedbackEvents are the webhook events applyGitHubFeedback handles.
var githubFeedbackEvents = map[string]bool{
	"pull_request_review": true, "pull_request_review_comment": true, "issue_comment": true,
	"check_run": true, "check_suite": true, "status": true,
}

type ghUser struct {
	Login string `json:"login"`
}

type ghPR struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

type ghApp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type ghFeedbackPayload struct {
	Action     string    `json:"action"`
	Repository *hookRepo `json:"repository"`
	// pull_request_review, pull_request_review_comment
	PullRequest *ghPR `json:"pull_request"`
	Review      *struct {
		ID       int64  `json:"id"`
		Body     string `json:"body"`
		State    string `json:"state"`
		HTMLURL  string `json:"html_url"`
		User     ghUser `json:"user"`
		CommitID string `json:"commit_id"`
	} `json:"review"`
	// pull_request_review_comment, issue_comment
	Comment *struct {
		ID               int64  `json:"id"`
		Body             string `json:"body"`
		HTMLURL          string `json:"html_url"`
		User             ghUser `json:"user"`
		Path             string `json:"path"`
		Line             *int   `json:"line"`
		OriginalLine     *int   `json:"original_line"`
		Side             string `json:"side"`
		SubjectType      string `json:"subject_type"`
		CommitID         string `json:"commit_id"`
		OriginalCommitID string `json:"original_commit_id"`
	} `json:"comment"`
	Issue *struct {
		Number      int       `json:"number"`
		HTMLURL     string    `json:"html_url"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Sender ghUser `json:"sender"`
	// check_run
	CheckRun *struct {
		Name         string `json:"name"`
		HeadSHA      string `json:"head_sha"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		HTMLURL      string `json:"html_url"`
		DetailsURL   string `json:"details_url"`
		App          ghApp  `json:"app"`
		PullRequests []ghPR `json:"pull_requests"`
	} `json:"check_run"`
	// check_suite
	CheckSuite *struct {
		HeadSHA      string `json:"head_sha"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		App          ghApp  `json:"app"`
		PullRequests []ghPR `json:"pull_requests"`
	} `json:"check_suite"`
	// status
	SHA       string `json:"sha"`
	State     string `json:"state"`
	Context   string `json:"context"`
	TargetURL string `json:"target_url"`
}

// applyGitHubFeedback applies a review or CI webhook to the vx pull
// request it is about; others are ignored.
func (s *Service) applyGitHubFeedback(ctx context.Context, event string, body []byte) error {
	var p ghFeedbackPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return errf(CodeInvalid, "the %s delivery does not parse: %v", event, err)
	}
	if p.Repository == nil {
		return nil
	}
	repo, err := s.Store.RepoByGitHubID(ctx, p.Repository.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch event {
	case "pull_request_review":
		if p.Review == nil || p.PullRequest == nil || (p.Action != "submitted" && p.Action != "edited") {
			return nil
		}
		state := strings.ToLower(p.Review.State)
		if state != ReviewApproved && state != ReviewChangesRequested && state != ReviewCommented {
			return nil // dismissed, pending
		}
		return s.ApplyReview(ctx, ReviewEvent{
			PR: githubPRRef(repo, p.PullRequest), Kind: ReviewSummary, State: state, ID: fmt.Sprint(p.Review.ID),
			Edited: p.Action == "edited", Reviewer: p.Review.User.Login, Body: p.Review.Body, URL: p.Review.HTMLURL,
			CommitSHA: p.Review.CommitID, Host: "GitHub",
		})

	case "pull_request_review_comment":
		c := p.Comment
		if c == nil || p.PullRequest == nil || (p.Action != "created" && p.Action != "edited") {
			return nil
		}
		line, commit := 0, c.CommitID
		switch {
		case c.SubjectType == "file" || strings.EqualFold(c.Side, "LEFT"):
			// The whole file, or a line the pull request removed: the change
			// that last touched the file gets it.
		case c.Line != nil:
			line = *c.Line
		case c.OriginalLine != nil: // outdated: the line as of the commit it was made on
			line, commit = *c.OriginalLine, c.OriginalCommitID
		}
		return s.ApplyReview(ctx, ReviewEvent{
			PR: githubPRRef(repo, p.PullRequest), Kind: ReviewLine, ID: fmt.Sprint(c.ID), Edited: p.Action == "edited",
			Reviewer: c.User.Login, Body: c.Body, URL: c.HTMLURL, Path: c.Path, Line: line, CommitSHA: commit, Host: "GitHub",
		})

	case "issue_comment":
		c := p.Comment
		if c == nil || p.Issue == nil || p.Issue.PullRequest == nil || (p.Action != "created" && p.Action != "edited") {
			return nil
		}
		if s.isOwnGitHubComment(ctx, c.User.Login) {
			return nil
		}
		return s.ApplyReview(ctx, ReviewEvent{
			PR:   PRRef{Repo: repo, Number: fmt.Sprint(p.Issue.Number), URL: p.Issue.HTMLURL},
			Kind: ReviewComment, ID: fmt.Sprint(c.ID), Edited: p.Action == "edited",
			Reviewer: c.User.Login, Body: c.Body, URL: c.HTMLURL, Host: "GitHub",
		})

	case "check_run":
		cr := p.CheckRun
		if cr == nil || cr.Name == EvidenceCheckName || s.isOwnGitHubApp(ctx, cr.App) {
			return nil
		}
		url := cr.HTMLURL
		if cr.DetailsURL != "" {
			url = cr.DetailsURL
		}
		return s.ApplyCI(ctx, CIEvent{
			Repo: repo, HeadSHA: cr.HeadSHA, PR: firstPR(repo, cr.PullRequests), Source: "check_run", Name: cr.Name,
			Status: cr.Status, Conclusion: cr.Conclusion, URL: url, Host: "GitHub",
		})

	case "check_suite":
		cs := p.CheckSuite
		if cs == nil || p.Action != "completed" || s.isOwnGitHubApp(ctx, cs.App) {
			return nil
		}
		name := cs.App.Name
		if name == "" {
			name = cs.App.Slug
		}
		return s.ApplyCI(ctx, CIEvent{
			Repo: repo, HeadSHA: cs.HeadSHA, PR: firstPR(repo, cs.PullRequests), Source: "check_suite", Name: name,
			Status: cs.Status, Conclusion: cs.Conclusion, Host: "GitHub",
		})

	case "status":
		if p.SHA == "" || p.Context == "" {
			return nil
		}
		status, conclusion := "completed", ""
		switch p.State {
		case "pending":
			status = "in_progress"
		case "success":
			conclusion = "success"
		case "failure":
			conclusion = "failure"
		default: // error
			conclusion = p.State
		}
		return s.ApplyCI(ctx, CIEvent{
			Repo: repo, HeadSHA: p.SHA, Source: "status", Name: p.Context, Status: status, Conclusion: conclusion, URL: p.TargetURL, Host: "GitHub",
		})
	}
	return nil
}

func githubPRRef(repo store.Repo, pr *ghPR) PRRef {
	return PRRef{Repo: repo, Number: fmt.Sprint(pr.Number), HeadBranch: pr.Head.Ref, HeadSHA: pr.Head.SHA, URL: pr.HTMLURL}
}

func firstPR(repo store.Repo, prs []ghPR) *PRRef {
	for i := range prs {
		if strings.HasPrefix(prs[i].Head.Ref, "vx/") {
			r := githubPRRef(repo, &prs[i])
			return &r
		}
	}
	return nil
}

// isOwnGitHubApp reports whether app is this server's App (its own check
// runs are not CI results).
func (s *Service) isOwnGitHubApp(ctx context.Context, app ghApp) bool {
	if app.ID == 0 && app.Slug == "" {
		return false
	}
	mine, err := s.GitHubApp(ctx)
	if err != nil {
		return false
	}
	return (app.ID != 0 && app.ID == mine.AppID) || (app.Slug != "" && app.Slug == mine.Slug)
}

// isOwnGitHubComment reports whether login is this server's App (its bot
// user is "<slug>[bot]").
func (s *Service) isOwnGitHubComment(ctx context.Context, login string) bool {
	slug, ok := strings.CutSuffix(login, "[bot]")
	if !ok {
		return false
	}
	mine, err := s.GitHubApp(ctx)
	return err == nil && mine.Slug == slug
}
