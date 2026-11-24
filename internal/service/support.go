package service

// Superadmins and support access (#33). The rules:
//
//   - A superadmin (principals.site_admin) runs the server: people,
//     invites and sign-up, the GitHub App, server settings, and the
//     operation log's metadata. Being one gives no role on any
//     repository: to a superadmin without a role, a repository is "not
//     found" like to anyone else.
//   - A repository's admins can give a superadmin support access: read or
//     write on that repository until a time, at most SupportMaxDuration
//     ahead. It is never admin, so it cannot be used to change who has
//     access. It counts on GitHub-backed repositories too, and only while
//     the person is still a superadmin.
//   - Grants and revocations are recorded in the operation log
//     (repo.support.grant, repo.support.revoke) and listed on the access
//     page as "operator (support access) until DATE". The repository's
//     admins revoke it; the superadmin may end their own early.
//   - Upgrading a server that had site admins opens the notice
//     "superadmin-review" (migration 029): the server logs it once, and the
//     admin panel shows it until a superadmin marks it reviewed.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/store"
)

const (
	// SupportDefaultDuration is how long support access lasts when the
	// grant names no duration.
	SupportDefaultDuration = 24 * time.Hour
	// SupportMaxDuration is the longest support access one grant gives.
	SupportMaxDuration = 30 * 24 * time.Hour
	// supportMinDuration keeps a typo (1s) from making a useless grant.
	supportMinDuration = 15 * time.Minute
)

// NoticeSuperadminReview is the upgrade notice of #33.
const NoticeSuperadminReview = "superadmin-review"

// SuperadminReviewText is what the server logs and the admin panel shows
// after the upgrade.
const SuperadminReviewText = "Site admins are now superadmins: they run the server (people, invites, sign-up, the GitHub App, server settings, the operation log) " +
	"and no longer have access to repositories. Grant support access per repository if needed: vx repo support REPO PERSON --role read --for 24h, " +
	"or on the repository's access page. Organization owners and admins keep their access."

// SupportGrant is a superadmin's time-limited role on a repository.
type SupportGrant struct {
	Repo      string
	Person    store.Principal
	Role      RepoRole
	ExpiresAt time.Time
	Reason    string
	GrantedBy string // ref of who granted it, "" when they are gone
	CreatedAt time.Time
}

// supportVia is how support access shows on the access page.
func supportVia(until time.Time) string {
	return "operator (support access) until " + until.UTC().Format("2006-01-02 15:04 UTC")
}

func supportFromStore(repo string, g store.SupportGrant) SupportGrant {
	out := SupportGrant{Repo: repo, Person: g.Person, Role: RepoRole(g.Role), ExpiresAt: g.ExpiresAt, Reason: g.Reason, CreatedAt: g.CreatedAt}
	if g.GrantedBy != nil {
		out.GrantedBy = g.GrantedBy.Ref()
	}
	return out
}

type supportChange struct {
	Person    string `json:"person"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
	Reason    string `json:"reason,omitempty"`
}

func supportChangeOf(g store.SupportGrant) supportChange {
	return supportChange{Person: g.Person.Ref(), Role: g.Role, ExpiresAt: g.ExpiresAt.UTC().Format(time.RFC3339), Reason: g.Reason}
}

// SupportInput is a support grant to make.
type SupportInput struct {
	Repo   string
	Person string        // handle or user:HANDLE of a superadmin
	Role   RepoRole      // read or write; empty: read
	For    time.Duration // 0: SupportDefaultDuration
	Reason string        // why, shown on the access page and in the log
}

// GrantSupport gives a superadmin support access to a repository
// (repository admins). Granting again replaces the earlier grant.
func (s *Service) GrantSupport(ctx context.Context, actor Actor, in SupportInput) (SupportGrant, error) {
	if err := peopleOnly(actor, "grant support access"); err != nil {
		return SupportGrant{}, err
	}
	role := in.Role
	if role == RoleNone {
		role = RoleRead
	}
	if role != RoleRead && role != RoleWrite {
		return SupportGrant{}, errf(CodeInvalid, "support access is read or write, not %q: it never includes admin, so it cannot change who has access. For more, grant a role: vx repo grant %s user:HANDLE --role admin", in.Role, in.Repo)
	}
	d := in.For
	if d == 0 {
		d = SupportDefaultDuration
	}
	if d < supportMinDuration || d > SupportMaxDuration {
		return SupportGrant{}, errf(CodeInvalid, "support access lasts from %s to %s (720h); %s is out of range: pass --for 24h, 72h or 168h, for example", supportMinDuration, "30 days", d)
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 200 {
		return SupportGrant{}, errf(CodeInvalid, "the reason is %d characters; keep it under 200", len(reason))
	}
	r, _, err := s.Authorize(ctx, actor, in.Repo, RoleAdmin)
	if err != nil {
		return SupportGrant{}, err
	}
	var out SupportGrant
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		p, err := s.resolvePerson(ctx, tx, in.Person)
		if err != nil {
			return err
		}
		if !p.SiteAdmin {
			return errf(CodeFailedPrecondition, "%s is not a superadmin of this server; support access is for the people who run it. Give %s a role instead: vx repo grant %s %s --role %s", p.Ref(), p.Ref(), r.Name, p.Ref(), role)
		}
		expires := s.now().Add(d).UTC().Truncate(time.Second)
		prev, err := tx.SetSupportGrant(ctx, r.ID, p.ID, string(role), expires, reason, &actor.ID)
		if err != nil {
			return err
		}
		g := store.SupportGrant{RepoID: r.ID, Person: p, Role: string(role), ExpiresAt: expires, Reason: reason, GrantedBy: &actor.Principal, CreatedAt: s.now()}
		out = supportFromStore(r.Name, g)
		var before any
		if prev != nil {
			before = supportChangeOf(*prev)
		}
		return tx.LogOp(ctx, &r.ID, &actor.ID, "repo.support.grant", r.Name, before, supportChangeOf(g))
	})
	if err != nil {
		return SupportGrant{}, classify(err)
	}
	return out, nil
}

// RevokeSupport ends a superadmin's support access to a repository: its
// admins may, and so may the superadmin for their own.
func (s *Service) RevokeSupport(ctx context.Context, actor Actor, repo, person string) error {
	if err := peopleOnly(actor, "revoke support access"); err != nil {
		return err
	}
	r, role, err := s.Authorize(ctx, actor, repo, RoleRead)
	if err != nil {
		return err
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		p, err := s.resolvePerson(ctx, tx, person)
		if err != nil {
			return err
		}
		if !role.Includes(RoleAdmin) && p.ID != actor.ID {
			return s.needRole(ctx, actor, r, role, RoleAdmin)
		}
		prev, err := tx.DeleteSupportGrant(ctx, r.ID, p.ID)
		if isHidden(classify(err)) {
			return errf(CodeNotFound, "%s has no support access to %s; `vx repo support %s` lists it", p.Ref(), r.Name, r.Name)
		}
		if err != nil {
			return err
		}
		return tx.LogOp(ctx, &r.ID, &actor.ID, "repo.support.revoke", r.Name, supportChangeOf(prev), nil)
	})
	return classify(err)
}

// ListSupport lists a repository's support access in effect (repository
// admins).
func (s *Service) ListSupport(ctx context.Context, actor Actor, repo string) ([]SupportGrant, error) {
	r, _, err := s.Authorize(ctx, actor, repo, RoleAdmin)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.ListSupportGrants(ctx, r.ID, s.now())
	if err != nil {
		return nil, err
	}
	out := make([]SupportGrant, len(rows))
	for i, g := range rows {
		out[i] = supportFromStore(r.Name, g)
	}
	return out, nil
}

// withSupportAccess adds support grants in effect on r to the access
// entries.
func (s *Service) withSupportAccess(ctx context.Context, r store.Repo, entries []AccessEntry) ([]AccessEntry, error) {
	rows, err := s.Store.ListSupportGrants(ctx, r.ID, s.now())
	if err != nil {
		return nil, err
	}
	for _, g := range rows {
		i := -1
		for j := range entries {
			if entries[j].Person.ID == g.Person.ID {
				i = j
				break
			}
		}
		if i < 0 {
			entries = append(entries, AccessEntry{Person: g.Person})
			i = len(entries) - 1
		}
		entries[i].Role = higher(entries[i].Role, RepoRole(g.Role))
		entries[i].Via = append(entries[i].Via, supportVia(g.ExpiresAt))
	}
	return entries, nil
}

// LogUpgradeNotices logs each open upgrade notice once, at server start.
func (s *Service) LogUpgradeNotices(ctx context.Context, logger *slog.Logger) error {
	n, err := s.Store.GetServerNotice(ctx, NoticeSuperadminReview)
	if isHidden(classify(err)) {
		return nil
	}
	if err != nil || n.ResolvedAt != nil {
		return err
	}
	first, err := s.Store.MarkServerNoticeLogged(ctx, NoticeSuperadminReview)
	if err != nil || !first {
		return err
	}
	logger.Warn("upgrade notice: "+SuperadminReviewText+" Review it on Admin > People; see docs/deploy.md, \"Upgrading to v0.5: superadmins\".",
		"notice", NoticeSuperadminReview)
	return nil
}

// SuperadminReviewPending reports whether the admin panel should show the
// upgrade review banner to actor: a superadmin, with the notice open.
func (s *Service) SuperadminReviewPending(ctx context.Context, actor Actor) bool {
	if !s.IsSuperadmin(ctx, actor) {
		return false
	}
	n, err := s.Store.GetServerNotice(ctx, NoticeSuperadminReview)
	return err == nil && n.ResolvedAt == nil
}

// ResolveSuperadminReview marks the upgrade review done (superadmins,
// people only). It is recorded in the operation log.
func (s *Service) ResolveSuperadminReview(ctx context.Context, actor Actor) error {
	if err := s.needSiteAdmin(ctx, actor, "mark the superadmin review done"); err != nil {
		return err
	}
	open, err := s.Store.ResolveServerNotice(ctx, NoticeSuperadminReview, actor.ID)
	if err != nil || !open {
		return err
	}
	return s.Store.LogOp(ctx, nil, &actor.ID, "server.notice.resolve", NoticeSuperadminReview, nil, map[string]any{"notice": NoticeSuperadminReview})
}

// String describes a grant in one line, for logs and CLI output.
func (g SupportGrant) String() string {
	return fmt.Sprintf("%s: %s on %s, %s", g.Person.Ref(), g.Role, g.Repo, supportVia(g.ExpiresAt))
}
