package githubtest

// Payload builders for review and CI webhooks on pull requests:
// pull_request_review, pull_request_review_comment, issue_comment,
// check_run, check_suite and status. Like the other builders they carry
// the fields GitHub sends that vx reads.

import "strconv"

// PR is a pull request as webhook payloads describe it.
type PR struct {
	Number  int
	Head    string // head branch, e.g. vx/rate-limiting
	HeadSHA string
	Base    string // default: the repository's default branch
}

func (s *Server) prJSON(r Repo, pr PR) map[string]any {
	base := pr.Base
	if base == "" {
		base = r.DefaultBranch
	}
	return map[string]any{
		"number":   pr.Number,
		"html_url": s.URL + "/" + r.FullName() + "/pull/" + itoa(pr.Number),
		"state":    "open",
		"head":     map[string]any{"ref": pr.Head, "sha": pr.HeadSHA},
		"base":     map[string]any{"ref": base},
	}
}

func (s *Server) hookBase(installationID int64, fullName string) (Repo, map[string]any) {
	r, _ := s.Repo(fullName)
	s.mu.Lock()
	defer s.mu.Unlock()
	return r, map[string]any{
		"repository":   s.hookRepoJSON(r),
		"installation": map[string]any{"id": installationID},
		"sender":       s.sender(),
	}
}

// ReviewEvent is a "pull_request_review" payload: action submitted, edited
// or dismissed; state approved, changes_requested or commented.
func (s *Server) ReviewEvent(installationID int64, fullName string, pr PR, action string, reviewID int64, reviewer, state, body string) map[string]any {
	r, p := s.hookBase(installationID, fullName)
	p["action"] = action
	p["pull_request"] = s.prJSON(r, pr)
	var b any = body
	if body == "" {
		b = nil // GitHub sends null for a review without a summary
	}
	p["review"] = map[string]any{
		"id": reviewID, "body": b, "state": state, "commit_id": pr.HeadSHA,
		"html_url": s.URL + "/" + fullName + "/pull/" + itoa(pr.Number) + "#pullrequestreview-" + itoa64(reviewID),
		"user":     map[string]any{"login": reviewer, "type": "User"},
	}
	return p
}

// ReviewComment is a line comment of a review.
type ReviewComment struct {
	ID       int64
	Reviewer string
	Body     string
	Path     string
	Line     int    // 0: a comment on the file
	Side     string // RIGHT (default) or LEFT
	CommitID string // default: the pull request's head
	// Outdated makes line null, as GitHub sends for a comment on a line a
	// later push changed: Line becomes original_line.
	Outdated bool
}

// ReviewCommentEvent is a "pull_request_review_comment" payload: action
// created, edited or deleted.
func (s *Server) ReviewCommentEvent(installationID int64, fullName string, pr PR, action string, c ReviewComment) map[string]any {
	r, p := s.hookBase(installationID, fullName)
	p["action"] = action
	p["pull_request"] = s.prJSON(r, pr)
	side := c.Side
	if side == "" {
		side = "RIGHT"
	}
	commit := c.CommitID
	if commit == "" {
		commit = pr.HeadSHA
	}
	comment := map[string]any{
		"id": c.ID, "body": c.Body, "path": c.Path, "side": side, "subject_type": "line",
		"commit_id": pr.HeadSHA, "original_commit_id": commit,
		"html_url": s.URL + "/" + fullName + "/pull/" + itoa(pr.Number) + "#discussion_r" + itoa64(c.ID),
		"user":     map[string]any{"login": c.Reviewer, "type": "User"},
		"line":     nil, "original_line": nil,
	}
	if c.Line == 0 {
		comment["subject_type"] = "file"
	} else {
		comment["original_line"] = c.Line
		if !c.Outdated {
			comment["line"] = c.Line
			comment["commit_id"] = commit
		}
	}
	p["comment"] = comment
	return p
}

// IssueCommentEvent is an "issue_comment" payload on pull request number:
// action created, edited or deleted.
func (s *Server) IssueCommentEvent(installationID int64, fullName string, number int, action string, commentID int64, author, body string) map[string]any {
	_, p := s.hookBase(installationID, fullName)
	p["action"] = action
	p["issue"] = map[string]any{
		"number": number, "html_url": s.URL + "/" + fullName + "/pull/" + itoa(number),
		"pull_request": map[string]any{"url": s.APIURL + "/repos/" + fullName + "/pulls/" + itoa(number)},
	}
	p["comment"] = map[string]any{
		"id": commentID, "body": body,
		"html_url": s.URL + "/" + fullName + "/pull/" + itoa(number) + "#issuecomment-" + itoa64(commentID),
		"user":     map[string]any{"login": author, "type": "User"},
	}
	return p
}

// CheckRunEvent is a "check_run" payload: action created, completed or
// rerequested; status queued, in_progress or completed. app is the CI's
// App slug (e.g. github-actions).
func (s *Server) CheckRunEvent(installationID int64, fullName string, pr PR, action, app, name, status, conclusion string) map[string]any {
	r, p := s.hookBase(installationID, fullName)
	p["action"] = action
	var concl any = conclusion
	if conclusion == "" {
		concl = nil
	}
	p["check_run"] = map[string]any{
		"id": s.nextCheckID(), "name": name, "head_sha": pr.HeadSHA, "status": status, "conclusion": concl,
		"html_url":      s.URL + "/" + fullName + "/runs/" + itoa(pr.Number),
		"details_url":   s.URL + "/" + fullName + "/actions/runs/" + itoa(pr.Number),
		"app":           map[string]any{"id": 15368, "slug": app, "name": app},
		"check_suite":   map[string]any{"head_branch": pr.Head, "head_sha": pr.HeadSHA},
		"pull_requests": []any{s.prJSON(r, pr)},
	}
	return p
}

// CheckSuiteEvent is a "check_suite" payload: action completed.
func (s *Server) CheckSuiteEvent(installationID int64, fullName string, pr PR, app, conclusion string) map[string]any {
	r, p := s.hookBase(installationID, fullName)
	p["action"] = "completed"
	p["check_suite"] = map[string]any{
		"id": s.nextCheckID(), "head_sha": pr.HeadSHA, "head_branch": pr.Head, "status": "completed", "conclusion": conclusion,
		"app":           map[string]any{"id": 15368, "slug": app, "name": app},
		"pull_requests": []any{s.prJSON(r, pr)},
	}
	return p
}

// StatusEvent is a "status" payload: state pending, success, failure or
// error, for commit sha.
func (s *Server) StatusEvent(installationID int64, fullName, sha, context, state, targetURL string) map[string]any {
	_, p := s.hookBase(installationID, fullName)
	p["sha"] = sha
	p["state"] = state
	p["context"] = context
	p["target_url"] = targetURL
	p["description"] = context + " " + state
	return p
}

func (s *Server) nextCheckID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id()
}

func itoa(n int) string { return strconv.Itoa(n) }

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
