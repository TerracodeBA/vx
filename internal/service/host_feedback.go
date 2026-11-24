package service

// Review comments and CI results from GitLab, Gitea, Forgejo and Bitbucket
// webhooks, mapped onto the host-neutral ReviewEvent and CIEvent of
// feedback.go. The Repo of each event is filled in per verified repository.

import (
	"encoding/json"
	"strconv"
	"strings"
)

// hostFeedback is the review and CI part of a delivery.
type hostFeedback struct {
	reviews []ReviewEvent
	ci      []CIEvent
}

func itoaID(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// gitlabCI maps a pipeline status to CIEvent Status and Conclusion.
func gitlabCI(status string) (string, string) {
	switch status {
	case "success":
		return "completed", "success"
	case "failed":
		return "completed", "failure"
	case "canceled", "canceling":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	case "manual":
		return "completed", "action_required"
	case "running":
		return "in_progress", ""
	}
	return "queued", "" // created, waiting_for_resource, preparing, pending, scheduled
}

func parseGitLabFeedback(body []byte) hostFeedback {
	var p struct {
		ObjectKind string `json:"object_kind"`
		User       struct {
			Username string `json:"username"`
		} `json:"user"`
		Project struct {
			WebURL string `json:"web_url"`
		} `json:"project"`
		Attrs struct {
			ID           int64  `json:"id"`
			IID          int64  `json:"iid"`
			Action       string `json:"action"`
			Note         string `json:"note"`
			NoteableType string `json:"noteable_type"`
			URL          string `json:"url"`
			Type         string `json:"type"` // DiffNote for line comments
			Status       string `json:"status"`
			SHA          string `json:"sha"`
			Name         string `json:"name"`
			SourceBranch string `json:"source_branch"`
			LastCommit   struct {
				ID string `json:"id"`
			} `json:"last_commit"`
			Position *struct {
				NewPath string `json:"new_path"`
				OldPath string `json:"old_path"`
				NewLine int    `json:"new_line"`
				HeadSHA string `json:"head_sha"`
			} `json:"position"`
		} `json:"object_attributes"`
		MergeRequest *struct {
			IID          int64  `json:"iid"`
			SourceBranch string `json:"source_branch"`
			URL          string `json:"url"`
			LastCommit   struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"merge_request"`
	}
	var fb hostFeedback
	if json.Unmarshal(body, &p) != nil {
		return fb
	}
	a := p.Attrs
	switch p.ObjectKind {
	case "note":
		if a.NoteableType != "MergeRequest" || p.MergeRequest == nil {
			return fb
		}
		ev := ReviewEvent{
			PR:   PRRef{Number: itoaID(p.MergeRequest.IID), HeadBranch: p.MergeRequest.SourceBranch, HeadSHA: p.MergeRequest.LastCommit.ID, URL: p.MergeRequest.URL},
			Kind: ReviewComment, ID: "gitlab-note-" + itoaID(a.ID), Edited: a.Action == "update",
			Reviewer: p.User.Username, Body: a.Note, URL: a.URL, Host: "GitLab",
		}
		if a.Type == "DiffNote" && a.Position != nil {
			ev.Kind, ev.Path, ev.Line, ev.CommitSHA = ReviewLine, a.Position.NewPath, a.Position.NewLine, a.Position.HeadSHA
			if ev.Path == "" {
				ev.Path = a.Position.OldPath
			}
		}
		fb.reviews = append(fb.reviews, ev)
	case "merge_request":
		if a.Action != "approved" {
			return fb
		}
		fb.reviews = append(fb.reviews, ReviewEvent{
			PR:   PRRef{Number: itoaID(a.IID), HeadBranch: a.SourceBranch, HeadSHA: a.LastCommit.ID, URL: a.URL},
			Kind: ReviewSummary, State: ReviewApproved, Reviewer: p.User.Username, URL: a.URL, Host: "GitLab",
		})
	case "pipeline":
		status, concl := gitlabCI(a.Status)
		ev := CIEvent{HeadSHA: a.SHA, Source: "pipeline", Name: "GitLab pipeline", Status: status, Conclusion: concl, Host: "GitLab"}
		if a.Name != "" {
			ev.Name = "GitLab pipeline " + a.Name
		}
		if p.Project.WebURL != "" && a.ID != 0 {
			ev.URL = p.Project.WebURL + "/-/pipelines/" + itoaID(a.ID)
		}
		if mr := p.MergeRequest; mr != nil && mr.IID != 0 {
			ev.PR = &PRRef{Number: itoaID(mr.IID), HeadBranch: mr.SourceBranch, HeadSHA: a.SHA, URL: mr.URL}
		}
		fb.ci = append(fb.ci, ev)
	}
	return fb
}

func parseGiteaFeedback(event string, body []byte, host string) hostFeedback {
	var p struct {
		Action string `json:"action"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		PullRequest *struct {
			Number  int64  `json:"number"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Review *struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"review"`
		Issue *struct {
			Number      int64           `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Comment *struct {
			ID      int64  `json:"id"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"comment"`
		// status
		SHA       string `json:"sha"`
		State     string `json:"state"`
		Context   string `json:"context"`
		TargetURL string `json:"target_url"`
	}
	var fb hostFeedback
	if json.Unmarshal(body, &p) != nil {
		return fb
	}
	switch event {
	case "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment":
		if p.PullRequest == nil {
			return fb
		}
		state := ReviewCommented
		switch event {
		case "pull_request_review_approved":
			state = ReviewApproved
		case "pull_request_review_rejected":
			state = ReviewChangesRequested
		}
		ev := ReviewEvent{
			PR:   PRRef{Number: itoaID(p.PullRequest.Number), HeadBranch: p.PullRequest.Head.Ref, HeadSHA: p.PullRequest.Head.SHA, URL: p.PullRequest.HTMLURL},
			Kind: ReviewSummary, State: state, Reviewer: p.Sender.Login, URL: p.PullRequest.HTMLURL, Host: host,
		}
		if p.Review != nil {
			ev.Body = p.Review.Content
		}
		fb.reviews = append(fb.reviews, ev)
	case "issue_comment", "pull_request_comment":
		if p.Comment == nil || (p.Action != "created" && p.Action != "edited") {
			return fb
		}
		var pr PRRef
		switch {
		case p.PullRequest != nil:
			pr = PRRef{Number: itoaID(p.PullRequest.Number), HeadBranch: p.PullRequest.Head.Ref, HeadSHA: p.PullRequest.Head.SHA, URL: p.PullRequest.HTMLURL}
		case p.Issue != nil && len(p.Issue.PullRequest) > 0 && string(p.Issue.PullRequest) != "null":
			pr = PRRef{Number: itoaID(p.Issue.Number)}
		default:
			return fb // a comment on an issue
		}
		fb.reviews = append(fb.reviews, ReviewEvent{
			PR: pr, Kind: ReviewComment, ID: strings.ToLower(host) + "-comment-" + itoaID(p.Comment.ID), Edited: p.Action == "edited",
			Reviewer: p.Comment.User.Login, Body: p.Comment.Body, URL: p.Comment.HTMLURL, Host: host,
		})
	case "status":
		ev := CIEvent{HeadSHA: p.SHA, Source: "status", Name: p.Context, URL: p.TargetURL, Host: host}
		switch p.State {
		case "success":
			ev.Status, ev.Conclusion = "completed", "success"
		case "failure", "error":
			ev.Status, ev.Conclusion = "completed", "failure"
		case "warning":
			ev.Status, ev.Conclusion = "completed", "neutral"
		default:
			ev.Status = "queued"
		}
		fb.ci = append(fb.ci, ev)
	}
	return fb
}

func parseBitbucketFeedback(event string, body []byte) hostFeedback {
	type user struct {
		Nickname    string `json:"nickname"`
		DisplayName string `json:"display_name"`
	}
	var p struct {
		Actor       user `json:"actor"`
		PullRequest struct {
			ID     int64 `json:"id"`
			Source struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
				Commit struct {
					Hash string `json:"hash"`
				} `json:"commit"`
			} `json:"source"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
		} `json:"pullrequest"`
		Comment *struct {
			ID      int64 `json:"id"`
			Content struct {
				Raw string `json:"raw"`
			} `json:"content"`
			Inline *struct {
				Path string `json:"path"`
				To   *int   `json:"to"`
			} `json:"inline"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
			User user `json:"user"`
		} `json:"comment"`
		CommitStatus *struct {
			Key    string `json:"key"`
			Name   string `json:"name"`
			State  string `json:"state"`
			URL    string `json:"url"`
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
			Links struct {
				Commit struct {
					Href string `json:"href"`
				} `json:"commit"`
			} `json:"links"`
		} `json:"commit_status"`
	}
	var fb hostFeedback
	if json.Unmarshal(body, &p) != nil {
		return fb
	}
	login := func(u user) string {
		if u.Nickname != "" {
			return u.Nickname
		}
		return u.DisplayName
	}
	pr := p.PullRequest
	ref := PRRef{Number: itoaID(pr.ID), HeadBranch: pr.Source.Branch.Name, HeadSHA: pr.Source.Commit.Hash, URL: pr.Links.HTML.Href}
	switch event {
	case "pullrequest:comment_created", "pullrequest:comment_updated":
		c := p.Comment
		if c == nil {
			return fb
		}
		ev := ReviewEvent{PR: ref, Kind: ReviewComment, ID: "bitbucket-comment-" + itoaID(c.ID), Edited: event == "pullrequest:comment_updated",
			Reviewer: login(c.User), Body: c.Content.Raw, URL: c.Links.HTML.Href, Host: "Bitbucket"}
		if c.Inline != nil && c.Inline.Path != "" {
			ev.Kind, ev.Path, ev.CommitSHA = ReviewLine, c.Inline.Path, pr.Source.Commit.Hash
			if c.Inline.To != nil {
				ev.Line = *c.Inline.To
			}
		}
		fb.reviews = append(fb.reviews, ev)
	case "pullrequest:approved":
		fb.reviews = append(fb.reviews, ReviewEvent{PR: ref, Kind: ReviewSummary, State: ReviewApproved, Reviewer: login(p.Actor), URL: ref.URL, Host: "Bitbucket"})
	case "pullrequest:changes_request_created":
		fb.reviews = append(fb.reviews, ReviewEvent{PR: ref, Kind: ReviewSummary, State: ReviewChangesRequested, Reviewer: login(p.Actor), URL: ref.URL, Host: "Bitbucket"})
	case "repo:commit_status_created", "repo:commit_status_updated":
		cs := p.CommitStatus
		if cs == nil {
			return fb
		}
		sha := cs.Commit.Hash
		if sha == "" {
			if i := strings.LastIndex(cs.Links.Commit.Href, "/"); i >= 0 {
				sha = cs.Links.Commit.Href[i+1:]
			}
		}
		name := cs.Name
		if name == "" {
			name = cs.Key
		}
		ev := CIEvent{HeadSHA: sha, Source: "status", Name: name, URL: cs.URL, Host: "Bitbucket"}
		switch cs.State {
		case "SUCCESSFUL":
			ev.Status, ev.Conclusion = "completed", "success"
		case "FAILED":
			ev.Status, ev.Conclusion = "completed", "failure"
		case "STOPPED":
			ev.Status, ev.Conclusion = "completed", "cancelled"
		default: // INPROGRESS
			ev.Status = "in_progress"
		}
		fb.ci = append(fb.ci, ev)
	}
	return fb
}
