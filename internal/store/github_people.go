package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// People linked to GitHub. A person is linked by GitHub's numeric user id,
// which never changes; the login is kept for display and can be renamed.
// These columns stay out of principalCols on purpose: several queries scan
// principalCols positionally.

// GitHubLink is the GitHub account a person signs in with.
type GitHubLink struct {
	PrincipalID uuid.UUID
	UserID      int64
	Login       string
}

// PrincipalByGitHubID looks a person up by their GitHub user id.
func (q Q) PrincipalByGitHubID(ctx context.Context, githubUserID int64) (Principal, error) {
	p, err := scanPrincipal(q.db.QueryRow(ctx, `select `+principalCols+` from principals where github_user_id = $1`, githubUserID))
	return p, notFound(err, fmt.Sprintf("person with GitHub user id %d", githubUserID))
}

// PrincipalByGitHubLogin looks a person up by the GitHub login last seen
// for them, ignoring case. Logins can be renamed: use it for display and
// invites, never to sign someone in.
func (q Q) PrincipalByGitHubLogin(ctx context.Context, login string) (Principal, error) {
	p, err := scanPrincipal(q.db.QueryRow(ctx, `select `+principalCols+` from principals where lower(github_login) = lower($1)`, login))
	return p, notFound(err, "person with GitHub login "+login)
}

// GitHubLinkOf returns the GitHub account linked to a principal; ok is false
// when there is none.
func (q Q) GitHubLinkOf(ctx context.Context, principalID uuid.UUID) (GitHubLink, bool, error) {
	l := GitHubLink{PrincipalID: principalID}
	var id *int64
	var login *string
	err := q.db.QueryRow(ctx, `select github_user_id, github_login from principals where id = $1`, principalID).Scan(&id, &login)
	if err != nil {
		return GitHubLink{}, false, notFound(err, "principal")
	}
	if id == nil {
		return GitHubLink{}, false, nil
	}
	l.UserID = *id
	if login != nil {
		l.Login = *login
	}
	return l, true, nil
}

// GitHubLinksOf returns the GitHub accounts linked to any of ids, by
// principal id; principals without one are left out.
func (q Q) GitHubLinksOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]GitHubLink, error) {
	rows, err := q.db.Query(ctx, `select id, github_user_id, coalesce(github_login, '') from principals
		where id = any($1) and github_user_id is not null`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]GitHubLink{}
	for rows.Next() {
		var l GitHubLink
		if err := rows.Scan(&l.PrincipalID, &l.UserID, &l.Login); err != nil {
			return nil, err
		}
		out[l.PrincipalID] = l
	}
	return out, rows.Err()
}

// LinkGitHub links a person to a GitHub account, or updates the login of
// the account they are already linked to. It returns ErrExists when that
// GitHub account is linked to someone else.
func (q Q) LinkGitHub(ctx context.Context, principalID uuid.UUID, githubUserID int64, login string) error {
	tag, err := q.db.Exec(ctx, `update principals set github_user_id = $2, github_login = $3
		where id = $1 and kind = 'human'`, principalID, githubUserID, login)
	if isUniqueViolation(err) {
		return fmt.Errorf("GitHub account %s is already linked to another person: %w", login, ErrExists)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("person: %w", ErrNotFound)
	}
	return nil
}

// SetDisplayName changes a principal's display name.
func (q Q) SetDisplayName(ctx context.Context, principalID uuid.UUID, name string) error {
	_, err := q.db.Exec(ctx, `update principals set display_name = $2 where id = $1`, principalID, name)
	return err
}

// HandleTaken reports whether a handle is used by a principal or an
// organization (people's handles and organization handles share one space).
func (q Q) HandleTaken(ctx context.Context, handle string) (bool, error) {
	var taken bool
	err := q.db.QueryRow(ctx, `select exists (select 1 from principals where handle = $1)
		or exists (select 1 from orgs where handle = $1)`, handle).Scan(&taken)
	return taken, err
}

// WelcomeDismissed reports whether a person has dismissed the welcome page.
func (q Q) WelcomeDismissed(ctx context.Context, principalID uuid.UUID) (bool, error) {
	var at *time.Time
	err := q.db.QueryRow(ctx, `select welcome_dismissed_at from principals where id = $1`, principalID).Scan(&at)
	return at != nil, notFound(err, "principal")
}

// SetWelcomeDismissed dismisses the welcome page, or brings it back.
func (q Q) SetWelcomeDismissed(ctx context.Context, principalID uuid.UUID, dismissed bool) error {
	_, err := q.db.Exec(ctx, `update principals set welcome_dismissed_at = case when $2 then now() end where id = $1`, principalID, dismissed)
	return err
}

// SignupInvite allows a GitHub login to sign up while sign-up is
// invite-only.
type SignupInvite struct {
	GitHubLogin string // lowercased
	InvitedBy   *uuid.UUID
	Note        string
	SiteAdmin   bool // the person becomes a site admin (invites from the server console only)
	CreatedAt   time.Time
	UsedAt      *time.Time
}

const inviteCols = `github_login, invited_by, note, site_admin, created_at, used_at`

func scanInvite(row interface{ Scan(...any) error }) (SignupInvite, error) {
	var i SignupInvite
	err := row.Scan(&i.GitHubLogin, &i.InvitedBy, &i.Note, &i.SiteAdmin, &i.CreatedAt, &i.UsedAt)
	return i, err
}

// CreateInvite invites a GitHub login, or updates the note of an open
// invite. It returns ErrExists when the invite was already used.
func (q Q) CreateInvite(ctx context.Context, login string, by *uuid.UUID, note string, siteAdmin bool) (SignupInvite, error) {
	login = strings.ToLower(login)
	i, err := scanInvite(q.db.QueryRow(ctx, `insert into signup_invites (github_login, invited_by, note, site_admin) values ($1, $2, $3, $4)
		on conflict (github_login) do update set note = excluded.note, invited_by = excluded.invited_by,
			site_admin = signup_invites.site_admin or excluded.site_admin
		where signup_invites.used_at is null
		returning `+inviteCols, login, by, note, siteAdmin))
	if errors.Is(err, pgx.ErrNoRows) {
		return SignupInvite{}, fmt.Errorf("invite for %s was already used: %w", login, ErrExists)
	}
	return i, err
}

// OpenInvite returns the open invite for login; ok is false when there is
// none.
func (q Q) OpenInvite(ctx context.Context, login string) (SignupInvite, bool, error) {
	i, err := scanInvite(q.db.QueryRow(ctx, `select `+inviteCols+` from signup_invites where github_login = $1 and used_at is null`, strings.ToLower(login)))
	if errors.Is(err, pgx.ErrNoRows) {
		return SignupInvite{}, false, nil
	}
	return i, err == nil, err
}

// UseInvite marks an open invite for login used; ok is false when there is
// no open invite.
func (q Q) UseInvite(ctx context.Context, login string) (ok bool, err error) {
	tag, err := q.db.Exec(ctx, `update signup_invites set used_at = now() where github_login = $1 and used_at is null`, strings.ToLower(login))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteInvite withdraws an open invite; ok is false when there is none.
func (q Q) DeleteInvite(ctx context.Context, login string) (ok bool, err error) {
	tag, err := q.db.Exec(ctx, `delete from signup_invites where github_login = $1 and used_at is null`, strings.ToLower(login))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListInvites returns invites newest first; open ones only unless all.
func (q Q) ListInvites(ctx context.Context, all bool) ([]SignupInvite, error) {
	rows, err := q.db.Query(ctx, `select `+inviteCols+` from signup_invites where $1 or used_at is null
		order by created_at desc, github_login`, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignupInvite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
