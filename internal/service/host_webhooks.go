package service

// Webhook deliveries from GitLab (/hooks/gitlab, X-Gitlab-Token), Gitea and
// Forgejo (/hooks/gitea, /hooks/forgejo, HMAC-SHA256 in X-Gitea-Signature
// or X-Forgejo-Signature) and Bitbucket Cloud (/hooks/bitbucket, HMAC in
// X-Hub-Signature, or ?secret= for hooks without a secret field). A push to
// a followed branch imports it; a merged vx pull request marks its effort
// merged. Both go through the host-neutral functions of upstream_events.go.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// HostDelivery is one webhook delivery as it arrived; nothing in it is
// trusted before the secret checks out.
type HostDelivery struct {
	Event     string // X-Gitlab-Event, X-Gitea-Event / X-Forgejo-Event, X-Event-Key
	ID        string // X-Gitlab-Event-UUID, X-Gitea-Delivery, X-Request-UUID; may be empty
	Token     string // X-Gitlab-Token
	Signature string // X-Gitea-Signature, X-Forgejo-Signature, X-Hub-Signature
	URLSecret string // ?secret= (Bitbucket hooks without a secret)
	Body      []byte
}

// hostEvent is what vx takes from a delivery.
type hostEvent struct {
	remote, webURL string
	pushed         []string // branches pushed to (not deleted)
	merged         bool     // a pull request was merged
	head, base     string   // its branches
}

func zeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

func parseGitLabEvent(body []byte) (hostEvent, error) {
	var p struct {
		ObjectKind string `json:"object_kind"`
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Project    struct {
			PathWithNamespace string `json:"path_with_namespace"`
			WebURL            string `json:"web_url"`
		} `json:"project"`
		Attrs struct {
			Action       string `json:"action"`
			State        string `json:"state"`
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return hostEvent{}, err
	}
	ev := hostEvent{remote: p.Project.PathWithNamespace, webURL: p.Project.WebURL}
	switch p.ObjectKind {
	case "push":
		if b, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok && !zeroSHA(p.After) {
			ev.pushed = []string{b}
		}
	case "merge_request":
		if p.Attrs.Action == "merge" || p.Attrs.State == "merged" {
			ev.merged, ev.head, ev.base = true, p.Attrs.SourceBranch, p.Attrs.TargetBranch
		}
	}
	return ev, nil
}

func parseGiteaEvent(event string, body []byte) (hostEvent, error) {
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
		PullRequest *struct {
			Merged bool `json:"merged"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return hostEvent{}, err
	}
	ev := hostEvent{remote: p.Repository.FullName, webURL: p.Repository.HTMLURL}
	switch event {
	case "push":
		if b, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok && !zeroSHA(p.After) {
			ev.pushed = []string{b}
		}
	case "pull_request":
		if p.Action == "closed" && p.PullRequest != nil && p.PullRequest.Merged {
			ev.merged, ev.head, ev.base = true, p.PullRequest.Head.Ref, p.PullRequest.Base.Ref
		}
	}
	return ev, nil
}

func parseBitbucketEvent(event string, body []byte) (hostEvent, error) {
	type ref struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	var p struct {
		Repository struct {
			FullName string `json:"full_name"`
			Links    struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
		} `json:"repository"`
		Push struct {
			Changes []struct {
				New *ref `json:"new"`
			} `json:"changes"`
		} `json:"push"`
		PullRequest struct {
			State  string `json:"state"`
			Source struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
			} `json:"source"`
			Destination struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
			} `json:"destination"`
		} `json:"pullrequest"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return hostEvent{}, err
	}
	ev := hostEvent{remote: p.Repository.FullName, webURL: p.Repository.Links.HTML.Href}
	switch event {
	case "repo:push":
		seen := map[string]bool{}
		for _, c := range p.Push.Changes {
			if c.New != nil && c.New.Type == "branch" && c.New.Name != "" && !seen[c.New.Name] {
				seen[c.New.Name] = true
				ev.pushed = append(ev.pushed, c.New.Name)
			}
		}
	case "pullrequest:fulfilled":
		ev.merged, ev.head, ev.base = true, p.PullRequest.Source.Branch.Name, p.PullRequest.Destination.Branch.Name
	}
	return ev, nil
}

// hmacHex checks a hex HMAC-SHA256 of body, with or without "sha256=".
func hmacHex(secret, signature string, body []byte) bool {
	sig := strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil || secret == "" || len(got) == 0 {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

func equalSecret(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// verifyHostDelivery checks d against one secret the way the host signs.
func verifyHostDelivery(kind store.Provider, secret string, d HostDelivery) bool {
	if secret == "" {
		return false
	}
	switch kind {
	case store.ProviderGitLab:
		return equalSecret(d.Token, secret)
	case store.ProviderGitea, store.ProviderForgejo:
		return hmacHex(secret, d.Signature, d.Body)
	case store.ProviderBitbucket:
		if d.Signature != "" {
			return hmacHex(secret, d.Signature, d.Body)
		}
		return equalSecret(d.URLSecret, secret)
	}
	return false
}

// onHost reports whether a delivery's repository address lies on repo's
// host, so two servers with the same owner/repo path are told apart.
func onHost(repo store.Repo, webURL string) bool {
	if webURL == "" || repo.BaseURL == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(webURL), strings.ToLower(strings.TrimRight(repo.BaseURL, "/"))+"/")
}

// HandleHostWebhook verifies and applies a delivery from GitLab
// (kind gitlab), Gitea or Forgejo (gitea, forgejo: both accept either) or
// Bitbucket (bitbucket). A delivery is applied to every vx repository that
// follows its upstream and whose secret (its own, else the server's for
// the host) verifies it.
func (s *Service) HandleHostWebhook(ctx context.Context, kind store.Provider, d HostDelivery) error {
	var providers []store.Provider
	var hdr string
	switch kind {
	case store.ProviderGitLab:
		providers, hdr = []store.Provider{store.ProviderGitLab}, "X-Gitlab-Token"
		if d.Token == "" {
			return errf(CodePermission, "unsigned delivery: set the webhook's secret token to the repository's webhook secret (X-Gitlab-Token)")
		}
	case store.ProviderGitea, store.ProviderForgejo:
		providers, hdr = []store.Provider{store.ProviderGitea, store.ProviderForgejo}, "X-Gitea-Signature"
		if d.Signature == "" {
			return errf(CodePermission, "unsigned delivery: set the webhook's secret to the repository's webhook secret (X-Gitea-Signature)")
		}
	case store.ProviderBitbucket:
		providers, hdr = []store.Provider{store.ProviderBitbucket}, "X-Hub-Signature"
		if d.Signature == "" && d.URLSecret == "" {
			return errf(CodePermission, "unsigned delivery: set the webhook's secret to the repository's webhook secret (X-Hub-Signature), or add ?secret=SECRET to its URL")
		}
	default:
		return errf(CodeInvalid, "vx has no webhook for %q", kind)
	}
	if !json.Valid(d.Body) {
		return errf(CodeInvalid, "the delivery is not JSON")
	}
	var ev hostEvent
	var err error
	switch kind {
	case store.ProviderGitLab:
		ev, err = parseGitLabEvent(d.Body)
	case store.ProviderBitbucket:
		ev, err = parseBitbucketEvent(d.Event, d.Body)
	default:
		ev, err = parseGiteaEvent(d.Event, d.Body)
	}
	if err != nil {
		return errf(CodeInvalid, "the %s delivery does not parse: %v", kind.HostName(), err)
	}
	var candidates []store.Repo
	if ev.remote != "" {
		all, err := s.Store.ReposByRemote(ctx, strings.Trim(ev.remote, "/"), providers...)
		if err != nil {
			return err
		}
		for _, r := range all {
			if onHost(r, ev.webURL) {
				candidates = append(candidates, r)
			}
		}
	}
	var verified []store.Repo
	for _, r := range candidates {
		sec, err := s.repoWebhookSecret(ctx, r)
		if err != nil {
			return err
		}
		if sec == "" {
			sec = s.Hosts.WebhookSecrets[r.Provider]
		}
		if verifyHostDelivery(kind, sec, d) {
			verified = append(verified, r)
		}
	}
	if len(verified) == 0 {
		if len(candidates) == 0 {
			for _, p := range providers {
				if verifyHostDelivery(kind, s.Hosts.WebhookSecrets[p], d) {
					return nil // signed by this server, for a repository vx does not follow
				}
			}
		}
		return errf(CodePermission, "the delivery's %s does not match the webhook secret of a repository vx follows; see `vx repo host NAME --rotate-webhook-secret`", hdr)
	}
	if d.ID != "" && len(d.ID) <= 100 {
		id := string(kind) + ":" + d.ID
		fresh, rerr := s.Store.RecordGitHubDelivery(ctx, id, d.Event, githubDeliveryKeep)
		if rerr != nil {
			return rerr
		}
		if !fresh {
			return ErrGitHubDeliveryApplied
		}
		defer func() {
			if err != nil {
				_ = s.Store.ForgetGitHubDelivery(context.WithoutCancel(ctx), id)
			}
		}()
	}
	for _, r := range verified {
		for _, b := range ev.pushed {
			if err = s.applyUpstreamPush(ctx, r, b, strings.ToLower(r.HostName())+" push"); err != nil {
				return err
			}
		}
		if ev.merged {
			if err = s.applyMergedPullRequest(ctx, r, ev.head, ev.base, r.HostName()); err != nil {
				return err
			}
		}
	}
	return nil
}
