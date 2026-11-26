package service

// Presence: whether an agent is working right now, so a question to one
// that finished does not wait unseen until it escalates, and claims of a
// workspace nobody works in stop holding code.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

const (
	// AgentActiveWindow: an agent that used its token this recently is
	// working.
	AgentActiveWindow = 15 * time.Minute
	// WorkspaceIdleAfter: a workspace that did nothing (sync, land, status,
	// a token use by its owner) for this long no longer counts as someone
	// working, and its claims lapse.
	WorkspaceIdleAfter = 2 * time.Hour
)

// Presence says whether a principal is working right now.
type Presence struct {
	Active bool
	// LastSeen is its latest token use or workspace activity; nil: never.
	LastSeen *time.Time
}

// presence reports whether p is working: it has an open workspace that did
// something within WorkspaceIdleAfter, or used a token within
// AgentActiveWindow.
func (s *Service) presence(ctx context.Context, p store.Principal) (Presence, error) {
	now := s.Clock()
	var out Presence
	seen := func(t time.Time) {
		if out.LastSeen == nil || t.After(*out.LastSeen) {
			out.LastSeen = &t
		}
	}
	tokens, err := s.Store.ListTokens(ctx, p.ID)
	if err != nil {
		return out, err
	}
	for _, t := range tokens {
		if t.LastUsedAt != nil {
			seen(*t.LastUsedAt)
			if now.Sub(*t.LastUsedAt) < AgentActiveWindow {
				out.Active = true
			}
		}
	}
	wss, err := s.Store.OpenWorkspacesOf(ctx, p.ID)
	if err != nil {
		return out, err
	}
	for _, w := range wss {
		at := w.LastActive()
		seen(at)
		if now.Sub(at) < WorkspaceIdleAfter {
			out.Active = true
		}
	}
	return out, nil
}

// AwayNotice is what the asker of q is told when the agent asked was not
// working: "agent:alice/backend is not working right now (last seen 40 min
// ago); the question also goes to user:alice." Empty otherwise.
func AwayNotice(q Note, now time.Time) string {
	if q.ToAway == nil || q.To == nil {
		return ""
	}
	seen := "never seen"
	if q.ToAway.LastSeen != nil {
		seen = "last seen " + Ago(now, *q.ToAway.LastSeen)
	}
	out := fmt.Sprintf("%s is not working right now (%s)", q.To.Ref(), seen)
	if q.CopiedTo != nil {
		out += "; the question also goes to " + q.CopiedTo.Ref()
	}
	return out + "."
}

// Ago says how long before now t was: "just now", "40 min ago", "3h ago",
// "2 days ago".
func Ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// lapseIdleClaims ends the claims of an effort whose open workspace has
// been idle for WorkspaceIdleAfter, and tells each owner on the board. It
// runs before claims are read, inside q's transaction when there is one.
func (s *Service) lapseIdleClaims(ctx context.Context, q store.Q, effortID uuid.UUID) error {
	now := s.Clock()
	rows, err := q.LapseIdleClaims(ctx, effortID, now, now.Add(-WorkspaceIdleAfter))
	if err != nil || len(rows) == 0 {
		return err
	}
	vx, err := s.systemPrincipal(ctx)
	if err != nil {
		return err
	}
	type key struct{ owner, ws uuid.UUID }
	var order []key
	byKey := map[key][]store.Claim{}
	for _, c := range rows {
		k := key{c.OwnerID, *c.WorkspaceID}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], c)
		if err := q.LogOp(ctx, &c.RepoID, &vx.ID, "claim.lapse", c.ID.String(), nil,
			map[string]any{"effort": c.EffortSlug, "targets": c.Targets, "workspace": c.WorkspaceID.String()}); err != nil {
			return err
		}
	}
	for _, k := range order {
		cs := byKey[k]
		ws, err := q.GetWorkspace(ctx, k.ws)
		if err != nil {
			return err
		}
		var targets []string
		for _, c := range cs {
			targets = append(targets, c.Targets...)
		}
		targets = uniqueSorted(targets)
		body := fmt.Sprintf("Your claim on %s lapsed: workspace %s (%q) did nothing (no sync, land, status or token use) for %s, so it no longer holds that code for you. Still on it? Claim again: vx claim %s",
			fileList(targets), ws.ChangeID, ws.Intent, durationWords(WorkspaceIdleAfter), strings.Join(targets, " "))
		owner := k.owner
		if len(targets) > MaxNoteTargets {
			targets = targets[:MaxNoteTargets]
		}
		if _, err := q.InsertNote(ctx, store.Note{
			EffortID: cs[0].EffortID, Kind: string(NoteClaim), Targets: targets, AuthorID: vx.ID, ToID: &owner,
			Body: finishNote(body), State: NoteOpen, CreatedAt: now, System: true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// durationWords says a whole duration the way people do: "2 hours".
func durationWords(d time.Duration) string {
	if d%time.Hour == 0 {
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}
