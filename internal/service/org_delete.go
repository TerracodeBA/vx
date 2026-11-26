package service

// Deleting an organization (#40).
//
//   - Owners delete (not admins, not superadmins as such, never agents), by
//     naming the organization's handle again to confirm. Personal
//     organizations go only with their person.
//   - Deleting hides the organization at once: the store leaves it and its
//     repositories out of every lookup and list (store.repoFrom), so every
//     repository operation on it is "not found" in the CLI, the API, the web
//     UI and MCP, before any role is computed; shipping, syncing and
//     webhooks for it stop. Its handle stays reserved.
//   - An owner restores it, unchanged, until the grace period ends
//     (OrgDeletionSettings.Grace, VX_ORG_DELETE_GRACE, default 30 days).
//   - After that the purger (org_purge.go) deletes everything of it. What
//     is left is the deletion record: id, handle, who asked, when deleted,
//     when purged, and counts. Owners download a confirmation from it
//     during the grace period; after the purge the server's operator
//     produces it (vx-server org deletion-record).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// DefaultOrgDeleteGrace is how long a deleted organization can be
// restored before it is purged.
const DefaultOrgDeleteGrace = 30 * 24 * time.Hour

// OrgDeletionSettings configure deleting organizations.
type OrgDeletionSettings struct {
	// Grace is how long owners can restore a deleted organization
	// (VX_ORG_DELETE_GRACE); 0 means DefaultOrgDeleteGrace. Negative
	// purges at the next run of the purger (no grace period).
	Grace time.Duration
	// PurgeInterval is how often each server looks for organizations to
	// purge; 0 means DefaultPurgeInterval.
	PurgeInterval time.Duration
}

func (o OrgDeletionSettings) grace() time.Duration {
	switch {
	case o.Grace == 0:
		return DefaultOrgDeleteGrace
	case o.Grace < 0:
		return 0
	}
	return o.Grace
}

// OrgDeletion is the record of an organization's deletion: what a
// deletion confirmation says. It names no repository.
type OrgDeletion struct {
	OrgID        uuid.UUID        `json:"org_id"`
	Org          string           `json:"org"`
	RequestedBy  string           `json:"requested_by"`
	DeletedAt    time.Time        `json:"deleted_at"`
	PurgeAfter   time.Time        `json:"purge_after"`
	PurgeStarted *time.Time       `json:"purge_started,omitempty"`
	PurgedAt     *time.Time       `json:"purged_at,omitempty"`
	PurgedBy     string           `json:"purged_by,omitempty"`
	Counts       map[string]int64 `json:"counts,omitempty"`
}

func deletionFromStore(d store.OrgDeletion) OrgDeletion {
	return OrgDeletion{
		OrgID: d.OrgID, Org: d.Handle, RequestedBy: d.RequestedBy, DeletedAt: d.DeletedAt, PurgeAfter: d.PurgeAfter,
		PurgeStarted: d.PurgeStarted, PurgedAt: d.PurgedAt, PurgedBy: d.PurgedBy, Counts: d.Counts,
	}
}

// countNames are the counts of a confirmation, in order, with how they
// read.
var countNames = []struct{ key, name string }{
	{"repositories", "repositories"},
	{"efforts", "efforts"},
	{"workspaces", "workspaces"},
	{"changes", "changes"},
	{"shipments", "shipments"},
	{"notes", "board notes"},
	{"claims", "claims"},
	{"tasks", "tasks"},
	{"rebases", "rebases"},
	{"reviews", "reviews"},
	{"reservations", "reservations"},
	{"grants", "repository grants"},
	{"support_grants", "support grants"},
	{"check_jobs", "check jobs"},
	{"ci_tokens", "CI tokens"},
	{"object_records", "object records"},
	{"blobs", "stored objects (files and trees no other repository uses)"},
	{"git_mappings", "git object mappings"},
	{"mirrors", "git mirrors"},
	{"members", "memberships"},
	{"teams", "teams"},
	{"github_installation_links", "GitHub App installation links"},
	{"dpa_acceptances", "data processing agreement acceptances"},
	{"check_versions", "versions of the organization checks"},
	{"operations", "operation log entries"},
}

// Confirmation is the deletion confirmation as plain text, for owners to
// keep and for the operator to send after the purge.
func (d OrgDeletion) Confirmation(server string) string {
	var b strings.Builder
	day := func(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04 UTC") }
	fmt.Fprintf(&b, "Deletion confirmation\n\n")
	if server != "" {
		fmt.Fprintf(&b, "Server:          %s\n", server)
	}
	fmt.Fprintf(&b, "Organization:    %s\n", d.Org)
	fmt.Fprintf(&b, "Organization id: %s\n", d.OrgID)
	fmt.Fprintf(&b, "Deleted by:      %s\n", d.RequestedBy)
	fmt.Fprintf(&b, "Deleted on:      %s\n", day(d.DeletedAt))
	switch {
	case d.PurgedAt != nil:
		fmt.Fprintf(&b, "Purged on:       %s\n\n", day(*d.PurgedAt))
		fmt.Fprintf(&b, "All data of the organization was deleted from the server: its repositories\nwith their files and history, efforts, changes, shipments, board notes,\nclaims, tasks, reviews, grants, teams and memberships, its git mirrors and\nthe stored objects no other repository uses. People's accounts stay.\n\nRemoved:\n")
		for _, c := range countNames {
			if n := d.Counts[c.key]; n > 0 {
				fmt.Fprintf(&b, "  %8d %s\n", n, c.name)
			}
		}
		fmt.Fprintf(&b, "\nBackups of the server expire on their own schedule; data in them is not\nrestored into vx.\n")
	case d.PurgeStarted != nil:
		fmt.Fprintf(&b, "Purge:           in progress since %s\n\nThe organization's data is being deleted now. It can no longer be restored.\n", day(*d.PurgeStarted))
	default:
		fmt.Fprintf(&b, "Purge:           scheduled for %s\n\n", day(d.PurgeAfter))
		fmt.Fprintf(&b, "The organization is hidden: its repositories cannot be read or changed by\nanyone. An owner can restore it unchanged until the purge with\n\n  vx org restore %s\n\nAfter the purge every repository, effort, change, note and stored object of\nit is deleted for good, and only this record is kept.\n", d.Org)
	}
	return b.String()
}

// DeleteOrg deletes an organization: it is hidden at once and purged
// after the grace period. Only owners may, by naming its handle again in
// confirm; personal organizations cannot be deleted this way.
func (s *Service) DeleteOrg(ctx context.Context, actor Actor, handle, confirm string) (OrgDeletion, error) {
	if err := peopleOnly(actor, "delete organizations"); err != nil {
		return OrgDeletion{}, err
	}
	handle = strings.ToLower(strings.TrimSpace(handle))
	var out OrgDeletion
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.loadOrg(ctx, tx, actor, handle)
		if err != nil {
			return err
		}
		if a.org.Personal {
			return errf(CodeFailedPrecondition, "%s is a personal organization: it is deleted only with its person. Move or delete its repositories instead", a.org.Handle)
		}
		if !a.isOwner() {
			who := "a " + string(a.role)
			if a.role == "" {
				who = "a superadmin, not an owner"
			}
			return errf(CodePermission, "deleting organization %s needs owner; %s is %s. Ask an owner (%s)",
				a.org.Handle, actor.Human().Ref(), who, s.orgOwnerRefs(ctx, a.org.ID))
		}
		if strings.ToLower(strings.TrimSpace(confirm)) != a.org.Handle {
			return errf(CodeInvalid, "to delete organization %s, type its handle to confirm: vx org delete %s --confirm %s", a.org.Handle, a.org.Handle, a.org.Handle)
		}
		repos, _, err := tx.OrgUsage(ctx, a.org.ID)
		if err != nil {
			return err
		}
		now := s.Clock()
		d, err := tx.MarkOrgDeleted(ctx, a.org, actor.Principal, now, now.Add(s.OrgDeletion.grace()))
		if err != nil {
			return classify(err)
		}
		out = deletionFromStore(d)
		return tx.LogOp(ctx, nil, &actor.ID, "org.delete", a.org.Handle, nil, map[string]any{
			"org_id": a.org.ID, "purge_after": d.PurgeAfter, "repositories": repos, "github": a.org.GitHubLogin,
		})
	})
	return out, err
}

// deletedOrgOwned loads a deleted, not yet purged organization whose owner
// the caller is; to anyone else it does not exist.
func (s *Service) deletedOrgOwned(ctx context.Context, q *store.Tx, actor Actor, handle string) (store.Org, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	var o store.Org
	var err error
	if q != nil {
		o, err = q.DeletedOrgByHandle(ctx, handle, true)
	} else {
		o, err = s.Store.DeletedOrgByHandle(ctx, handle, false)
	}
	if err != nil {
		return store.Org{}, classify(err)
	}
	var role string
	if q != nil {
		role, err = q.OrgRole(ctx, o.ID, actor.Human().ID)
	} else {
		role, err = s.Store.OrgRole(ctx, o.ID, actor.Human().ID)
	}
	if err != nil {
		return store.Org{}, err
	}
	if OrgRole(role) != OrgOwner {
		return store.Org{}, orgNotFound(handle)
	}
	return o, nil
}

// RestoreOrg brings a deleted organization back, unchanged, while its
// grace period lasts. Only its owners may.
func (s *Service) RestoreOrg(ctx context.Context, actor Actor, handle string) (Org, error) {
	if err := peopleOnly(actor, "restore organizations"); err != nil {
		return Org{}, err
	}
	var out store.Org
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		o, err := s.deletedOrgOwned(ctx, tx, actor, handle)
		if err != nil {
			return err
		}
		d, err := tx.OrgDeletionByID(ctx, o.ID)
		if err != nil {
			return classify(err)
		}
		if !s.Clock().Before(d.PurgeAfter) {
			return errf(CodeFailedPrecondition, "the grace period of %s ended on %s and it is being purged; it can no longer be restored",
				o.Handle, d.PurgeAfter.UTC().Format(time.RFC3339))
		}
		if err := tx.RestoreDeletedOrg(ctx, o.ID); err != nil {
			if errors.Is(err, store.ErrPurgeStarted) {
				return errf(CodeFailedPrecondition, "%s is being purged and can no longer be restored", o.Handle)
			}
			return classify(err)
		}
		out = o
		return tx.LogOp(ctx, nil, &actor.ID, "org.restore", o.Handle, map[string]any{"org_id": o.ID, "deleted_at": d.DeletedAt, "requested_by": d.RequestedBy}, nil)
	})
	return orgFromStore(out), err
}

// OrgDeletionRecord returns the deletion record of a deleted organization
// the caller owns, before its purge.
func (s *Service) OrgDeletionRecord(ctx context.Context, actor Actor, handle string) (OrgDeletion, error) {
	if err := peopleOnly(actor, "read deletion records"); err != nil {
		return OrgDeletion{}, err
	}
	o, err := s.deletedOrgOwned(ctx, nil, actor, handle)
	if err != nil {
		return OrgDeletion{}, err
	}
	d, err := s.Store.OrgDeletionByID(ctx, o.ID)
	if err != nil {
		return OrgDeletion{}, classify(err)
	}
	return deletionFromStore(d), nil
}

// DeletedOrgs lists the deleted organizations the caller owns that can
// still be restored or are being purged, soonest purge first.
func (s *Service) DeletedOrgs(ctx context.Context, actor Actor) ([]OrgDeletion, error) {
	if actor.Human().Kind != store.KindHuman {
		return nil, nil
	}
	_, dels, err := s.Store.DeletedOrgsOwnedBy(ctx, actor.Human().ID)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]OrgDeletion, 0, len(dels))
	for _, d := range dels {
		out = append(out, deletionFromStore(d))
	}
	return out, nil
}

// ConsoleOrgDeletionRecords returns every deletion record of organizations
// that had handle, newest first, for the server's operator (vx-server org
// deletion-record): after the purge nobody else can see them.
func (s *Service) ConsoleOrgDeletionRecords(ctx context.Context, handle string) ([]OrgDeletion, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	rows, err := s.Store.OrgDeletionsByHandle(ctx, handle)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errf(CodeNotFound, "no deletion record for organization %s: it was never deleted, or was restored", handle)
	}
	out := make([]OrgDeletion, 0, len(rows))
	for _, d := range rows {
		out = append(out, deletionFromStore(d))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	return out, nil
}
