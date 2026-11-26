package service

// Explaining an empty account (#44). Someone who signs in with GitHub in a
// browser logged in to a second GitHub account gets a second, empty vx
// account; the pages then show nothing and it looks like the data is gone.
// AccessHelp collects what vx knows about why an account reads nothing, so
// the empty pages can say which account is signed in, why, and what to do.

import (
	"context"
	"time"

	"github.com/TerracodeBA/vx/internal/store"
)

// NewAccountWindow is how recently an account must have been created for
// the empty pages to say "created minutes ago": a sign-in with another
// GitHub account than usual creates one.
const NewAccountWindow = time.Hour

// AccessHelp is what vx knows about an account that can read no
// repositories.
type AccessHelp struct {
	// Person is the signed-in person (an agent's person for agents).
	Person store.Principal
	Agent  bool // the caller is an agent of Person
	// GitHubLogin is the linked GitHub login; "" when not connected.
	GitHubLogin string
	// GitHubSignIn says the server signs people in with GitHub (it has an
	// App); InstallURL is where the App is installed, "" without one.
	GitHubSignIn bool
	InstallURL   string
	GitHubURL    string // github.com or the GitHub Enterprise address
	// Orgs are the vx organizations the person belongs to other than
	// their personal one.
	Orgs []string
	// Installations counts the App's installations on the person's GitHub
	// account and on GitHub organizations GitHub says they belong to;
	// meaningful only with a GitHubLogin.
	Installations int
	// NewAccount is true when the person was created within
	// NewAccountWindow; Created is when.
	NewAccount bool
	Created    time.Time
}

// AccessHelp explains, for the signed-in actor, why they can read no
// repositories. It does not check that they read none: callers show it
// only when a list of readable repositories came back empty. Asking GitHub
// is best effort; what it cannot find out is left at its zero value.
func (s *Service) AccessHelp(ctx context.Context, actor Actor) (AccessHelp, error) {
	p := actor.Human()
	if fresh, err := s.Store.PrincipalByID(ctx, p.ID); err == nil {
		p = fresh
	}
	h := AccessHelp{Person: p, Agent: actor.Parent != nil, GitHubURL: s.GitHubWebURL(), Created: p.CreatedAt}
	h.NewAccount = !p.CreatedAt.IsZero() && s.Clock().Sub(p.CreatedAt) < NewAccountWindow
	rows, err := s.Store.ListOrgs(ctx, p.ID, false)
	if err != nil {
		return AccessHelp{}, classify(err)
	}
	for _, o := range rows {
		if !o.Org.Personal {
			h.Orgs = append(h.Orgs, o.Org.Handle)
		}
	}
	h.GitHubSignIn = s.GitHubSignInEnabled(ctx)
	if h.GitHubSignIn {
		if u, err := s.GitHubAppInstallURL(ctx); err == nil {
			h.InstallURL = u
		}
	}
	if l, ok, err := s.Store.GitHubLinkOf(ctx, p.ID); err == nil && ok {
		h.GitHubLogin = l.Login
		if inst, err := s.MyGitHubInstallations(ctx, actor); err == nil {
			for _, i := range inst {
				if i.SuspendedAt == nil {
					h.Installations++
				}
			}
		}
	}
	return h, nil
}
