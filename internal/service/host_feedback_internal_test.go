package service

import "testing"

// Gitea, Forgejo and Bitbucket review and CI deliveries become
// ReviewEvent and CIEvent (GitLab is covered end to end in the CLI).
func TestHostFeedbackParsing(t *testing.T) {
	g := parseGiteaFeedback("pull_request_review_rejected", []byte(`{"action":"reviewed","sender":{"login":"rev"},
		"pull_request":{"number":5,"html_url":"https://g/acme/shop/pulls/5","head":{"ref":"vx/a","sha":"abc"}},"review":{"type":"rejected","content":"Needs tests."}}`), "Forgejo")
	if len(g.reviews) != 1 || g.reviews[0].State != ReviewChangesRequested || g.reviews[0].PR.Number != "5" || g.reviews[0].Body != "Needs tests." || g.reviews[0].Reviewer != "rev" {
		t.Fatalf("gitea review: %+v", g.reviews)
	}
	g = parseGiteaFeedback("issue_comment", []byte(`{"action":"created","issue":{"number":5,"pull_request":{"merged":false}},
		"comment":{"id":42,"body":"Why?","html_url":"u","user":{"login":"rev"}}}`), "Gitea")
	if len(g.reviews) != 1 || g.reviews[0].Kind != ReviewComment || g.reviews[0].ID != "gitea-comment-42" || g.reviews[0].PR.Number != "5" {
		t.Fatalf("gitea comment: %+v", g.reviews)
	}
	if g := parseGiteaFeedback("issue_comment", []byte(`{"action":"created","issue":{"number":6,"pull_request":null},"comment":{"id":1,"body":"x"}}`), "Gitea"); len(g.reviews) != 0 {
		t.Fatalf("an issue comment became a review: %+v", g.reviews)
	}
	g = parseGiteaFeedback("status", []byte(`{"sha":"abc","state":"error","context":"ci/test","target_url":"https://ci/1"}`), "Gitea")
	if len(g.ci) != 1 || g.ci[0].Conclusion != "failure" || g.ci[0].Name != "ci/test" || g.ci[0].HeadSHA != "abc" {
		t.Fatalf("gitea status: %+v", g.ci)
	}

	pr := `"pullrequest":{"id":9,"source":{"branch":{"name":"vx/a"},"commit":{"hash":"def"}},"links":{"html":{"href":"https://bitbucket.org/acme/shop/pull-requests/9"}}}`
	b := parseBitbucketFeedback("pullrequest:comment_created", []byte(`{`+pr+`,"comment":{"id":3,"content":{"raw":"Off by one."},"inline":{"path":"limit.go","to":12},"user":{"nickname":"rev"}}}`))
	if len(b.reviews) != 1 || b.reviews[0].Kind != ReviewLine || b.reviews[0].Path != "limit.go" || b.reviews[0].Line != 12 || b.reviews[0].PR.HeadBranch != "vx/a" {
		t.Fatalf("bitbucket line comment: %+v", b.reviews)
	}
	b = parseBitbucketFeedback("pullrequest:approved", []byte(`{`+pr+`,"actor":{"nickname":"rev"}}`))
	if len(b.reviews) != 1 || b.reviews[0].State != ReviewApproved || b.reviews[0].Reviewer != "rev" {
		t.Fatalf("bitbucket approval: %+v", b.reviews)
	}
	b = parseBitbucketFeedback("repo:commit_status_updated", []byte(`{"commit_status":{"key":"build","name":"Pipelines","state":"FAILED","url":"https://ci",
		"links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/acme/shop/commit/def"}}}}`))
	if len(b.ci) != 1 || b.ci[0].HeadSHA != "def" || b.ci[0].Conclusion != "failure" || b.ci[0].Name != "Pipelines" {
		t.Fatalf("bitbucket status: %+v", b.ci)
	}
}
