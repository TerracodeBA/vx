package githubtest

import (
	"strings"
	"time"
)

// Webhook payload builders. They read the fake's current state, so build
// an installation "deleted" payload before DeleteInstallation, and a
// "renamed" one after RenameRepo. Payloads carry the fields GitHub sends
// that vx reads; deliver them with Deliver.

func (s *Server) accountJSON(login string) map[string]any {
	a, ok := s.accounts[strings.ToLower(login)]
	if !ok {
		return map[string]any{"login": login}
	}
	return map[string]any{"id": a.ID, "login": a.Login, "type": a.Type}
}

func (s *Server) hookRepoJSON(r Repo) map[string]any {
	return map[string]any{
		"id": r.ID, "name": r.Name, "full_name": r.FullName(), "private": r.Private,
		"default_branch": r.DefaultBranch, "owner": s.accountJSON(r.Owner),
	}
}

func (s *Server) sender() map[string]any {
	return map[string]any{"login": "octocat", "id": 1, "type": "User"}
}

// InstallationEvent is an "installation" payload: created, deleted,
// suspend, unsuspend, new_permissions_accepted.
func (s *Server) InstallationEvent(action string, id int64) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.mustInstall(id)
	repos := []map[string]any{}
	for _, r := range s.installReposLocked(in) {
		repos = append(repos, s.hookRepoJSON(*r))
	}
	return map[string]any{
		"action":       action,
		"installation": s.installationJSON(in),
		"repositories": repos,
		"sender":       s.sender(),
	}
}

// InstallationReposEvent is an "installation_repositories" payload:
// added or removed, listing repos (full names).
func (s *Server) InstallationReposEvent(action string, id int64, repos ...string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.mustInstall(id)
	list := []map[string]any{}
	for _, full := range repos {
		list = append(list, s.hookRepoJSON(*s.mustRepo(full)))
	}
	sel := "selected"
	if in.All {
		sel = "all"
	}
	p := map[string]any{
		"action":               action,
		"installation":         s.installationJSON(in),
		"repository_selection": sel,
		"repositories_added":   []map[string]any{},
		"repositories_removed": []map[string]any{},
		"sender":               s.sender(),
	}
	if action == "added" {
		p["repositories_added"] = list
	} else {
		p["repositories_removed"] = list
	}
	return p
}

// RepositoryEvent is a "repository" payload for r (from Repo, or the value
// DeleteRepo returned): deleted, privatized, publicized, edited, archived.
func (s *Server) RepositoryEvent(action string, installationID int64, r Repo) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"action":       action,
		"repository":   s.hookRepoJSON(r),
		"installation": map[string]any{"id": installationID},
		"sender":       s.sender(),
	}
}

// RepoRenamedEvent is a "repository" renamed payload; call it after
// RenameRepo with the old name.
func (s *Server) RepoRenamedEvent(installationID int64, fullName, oldName string) map[string]any {
	r, _ := s.Repo(fullName)
	p := s.RepositoryEvent("renamed", installationID, r)
	p["changes"] = map[string]any{"repository": map[string]any{"name": map[string]any{"from": oldName}}}
	return p
}

// RepoTransferredEvent is a "repository" transferred payload; call it
// after TransferRepo with the old owner.
func (s *Server) RepoTransferredEvent(installationID int64, fullName, oldOwner string) map[string]any {
	r, _ := s.Repo(fullName)
	p := s.RepositoryEvent("transferred", installationID, r)
	s.mu.Lock()
	from := s.accountJSON(oldOwner)
	s.mu.Unlock()
	key := "user"
	if from["type"] == "Organization" {
		key = "organization"
	}
	p["changes"] = map[string]any{"owner": map[string]any{"from": map[string]any{key: from}}}
	return p
}

// PushEvent is a "push" payload to ref (e.g. refs/heads/main).
func (s *Server) PushEvent(installationID int64, fullName, ref, before, after string) map[string]any {
	r, _ := s.Repo(fullName)
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"ref": ref, "before": before, "after": after, "created": false, "deleted": false,
		"repository":   s.hookRepoJSON(r),
		"installation": map[string]any{"id": installationID},
		"pusher":       map[string]any{"name": "octocat"},
		"sender":       s.sender(),
		"commits":      []any{},
		"head_commit":  map[string]any{"id": after, "timestamp": time.Now().UTC().Format(time.RFC3339)},
	}
}

// MemberEvent is a "member" payload (a repository collaborator): added,
// removed or edited.
func (s *Server) MemberEvent(installationID int64, action, fullName, login string) map[string]any {
	r, _ := s.Repo(fullName)
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"action":       action,
		"member":       s.accountJSON(login),
		"repository":   s.hookRepoJSON(r),
		"installation": map[string]any{"id": installationID},
		"sender":       s.sender(),
	}
}

// OrgMemberEvent is an "organization" payload: member_added,
// member_removed or member_invited.
func (s *Server) OrgMemberEvent(installationID int64, action, org, login, role string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"action":       action,
		"membership":   map[string]any{"user": s.accountJSON(login), "role": role, "state": "active"},
		"organization": s.accountJSON(org),
		"installation": map[string]any{"id": installationID},
		"sender":       s.sender(),
	}
}

// MembershipEvent is a "membership" payload (team membership): added or
// removed.
func (s *Server) MembershipEvent(installationID int64, action, org, team, login string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"action":       action,
		"scope":        "team",
		"member":       s.accountJSON(login),
		"team":         map[string]any{"slug": team, "name": team},
		"organization": s.accountJSON(org),
		"installation": map[string]any{"id": installationID},
		"sender":       s.sender(),
	}
}

// TeamEvent is a "team" payload: created, deleted, edited,
// added_to_repository or removed_from_repository (fullName may be "").
func (s *Server) TeamEvent(installationID int64, action, org, team, fullName string) map[string]any {
	var repo map[string]any
	if fullName != "" {
		r, _ := s.Repo(fullName)
		s.mu.Lock()
		repo = s.hookRepoJSON(r)
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := map[string]any{
		"action":       action,
		"team":         map[string]any{"slug": team, "name": team},
		"organization": s.accountJSON(org),
		"installation": map[string]any{"id": installationID},
		"sender":       s.sender(),
	}
	if repo != nil {
		p["repository"] = repo
	}
	return p
}

// TeamAddEvent is a "team_add" payload: a team got access to a repository.
func (s *Server) TeamAddEvent(installationID int64, org, team, fullName string) map[string]any {
	p := s.TeamEvent(installationID, "", org, team, fullName)
	delete(p, "action")
	return p
}
