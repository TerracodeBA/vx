package service

// Purging deleted organizations (#40). Every server runs RunOrgPurger; one
// at a time purges (a Postgres session advisory lock: when its server
// dies, the lock goes with the connection and another server takes over).
// The purge works in small batches, each its own short transaction, in
// this order, and records the step it is at and what it removed, so a
// purge that stopped (crash, deploy, lost lock) continues where it was:
//
//  1. repositories: for each, its object records in batches (marking the
//     blobs no repository outside the organization records), then git
//     mappings, push markers, file changes, its operation log entries,
//     notes and changes in batches, then the repository row, which
//     cascades to the small rest (efforts, workspaces, claims, tasks,
//     rebases, overlaps, reviews, reservations, shipments, grants, support
//     grants, cached GitHub roles, branches); and its git mirror here.
//  2. blobs: every marked blob that still no repository records is deleted
//     from the blob store with its recorded size (store.SweepBlobs). This
//     waits while another repository's objects are not indexed yet
//     (object backfill), because its objects would look unused.
//  3. the organization: members, teams, quotas, single sign-on, the GitHub
//     installation link, its own operation log entries (not org.delete,
//     org.restore, org.purge), then the row, which frees its handle; the
//     deletion record is marked purged and org.purge logged.
//
// Git mirrors of the organization on other servers are removed by their
// own purger loop, which deletes mirrors of repositories that no longer
// exist.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/store"
)

// DefaultPurgeInterval is how often servers look for organizations to
// purge.
const DefaultPurgeInterval = 10 * time.Minute

// blobMarkKeep is how long the mark of a deleted blob is kept, so a
// recording that started before the deletion still sees it.
const blobMarkKeep = 24 * time.Hour

// MirrorCleaner is an UpstreamSyncer that keeps git mirrors on this
// server's disk (the git bridge).
type MirrorCleaner interface {
	// RemoveMirror deletes the mirror of a repository; false when there was
	// none.
	RemoveMirror(repoID uuid.UUID) (bool, error)
	// MirrorRepoIDs lists the repositories with a mirror here.
	MirrorRepoIDs() ([]uuid.UUID, error)
}

// ErrPurgeBusy means another server holds the purge lock.
var ErrPurgeBusy = errors.New("another server is purging organizations right now")

// errPurgeWaiting means a purge paused until other repositories' objects
// are indexed; the next run continues it.
var errPurgeWaiting = errors.New("waiting for the object backfill of other repositories")

// RunOrgPurger purges deleted organizations whose grace period ended, and
// removes git mirrors of purged repositories, until ctx ends.
func (s *Service) RunOrgPurger(ctx context.Context, log *slog.Logger) {
	every := s.OrgDeletion.PurgeInterval
	if every <= 0 {
		every = DefaultPurgeInterval
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := s.SweepMirrors(ctx); err != nil && ctx.Err() == nil {
			log.Warn("could not remove git mirrors of purged repositories", "err", err)
		} else if n > 0 {
			log.Info("removed git mirrors of purged repositories", "count", n)
		}
		done, err := s.PurgeDueOrgs(ctx, log)
		switch {
		case errors.Is(err, ErrPurgeBusy):
		case err != nil && ctx.Err() == nil:
			log.Warn("organization purge stopped; the next run continues it", "err", err)
		case len(done) > 0:
			for _, d := range done {
				log.Info("purged organization", "org", d.Org, "org_id", d.OrgID, "counts", d.Counts)
			}
		}
		t.Reset(every)
	}
}

// PurgeDueOrgs purges every deleted organization whose grace period ended
// (and finishes purges that stopped), if no other server is purging.
func (s *Service) PurgeDueOrgs(ctx context.Context, log *slog.Logger) ([]OrgDeletion, error) {
	alive, unlock, ok, err := s.Store.TryPurgeLock(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrPurgeBusy
	}
	defer unlock()
	due, err := s.Store.OrgsDueForPurge(ctx, s.now())
	if err != nil {
		return nil, err
	}
	var done []OrgDeletion
	var errs []error
	for _, id := range due {
		d, err := s.purgeOrg(ctx, alive, id, "schedule")
		if err != nil {
			if errors.Is(err, errPurgeWaiting) {
				if log != nil {
					log.Info("organization purge waits for the object backfill of other repositories", "org_id", id)
				}
				continue
			}
			errs = append(errs, fmt.Errorf("organization %s: %w", id, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		done = append(done, d)
	}
	return done, errors.Join(errs...)
}

// ConsolePurgeOrg purges a deleted organization now, before its grace
// period ends, for the server's operator (vx-server org purge --now): for
// a customer who asks for their data to go at once.
func (s *Service) ConsolePurgeOrg(ctx context.Context, handle string) (OrgDeletion, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	o, err := s.Store.DeletedOrgByHandle(ctx, handle, false)
	if err != nil {
		if _, live := s.Store.OrgByHandle(ctx, handle); live == nil {
			return OrgDeletion{}, errf(CodeFailedPrecondition, "organization %s is not deleted: an owner deletes it first (vx org delete %s), then it can be purged at once", handle, handle)
		}
		return OrgDeletion{}, errf(CodeNotFound, "no deleted organization %s waits for a purge; vx-server org deletion-record %s shows earlier ones", handle, handle)
	}
	alive, unlock, ok, err := s.Store.TryPurgeLock(ctx)
	if err != nil {
		return OrgDeletion{}, err
	}
	if !ok {
		return OrgDeletion{}, errf(CodeUnavailable, "%v; try again in a minute", ErrPurgeBusy)
	}
	defer unlock()
	d, err := s.purgeOrg(ctx, alive, o.ID, "console")
	if errors.Is(err, errPurgeWaiting) {
		return d, errf(CodeUnavailable, "the repositories of %s are deleted; deleting its stored objects %v, and the server's purger finishes it then", handle, errPurgeWaiting)
	}
	return d, err
}

// purgeOrg runs (or continues) the purge of one deleted organization. The
// caller holds the purge lock.
func (s *Service) purgeOrg(ctx context.Context, alive func(context.Context) bool, orgID uuid.UUID, by string) (OrgDeletion, error) {
	t, err := s.Store.PurgeTarget(ctx, orgID, by, s.now())
	if err != nil {
		return OrgDeletion{}, err
	}
	if t.Deletion.PurgedAt != nil {
		return deletionFromStore(t.Deletion), nil
	}
	batch := func(step string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !alive(ctx) {
			return errors.New("lost the purge lock (database connection)")
		}
		if s.purgeHook != nil {
			s.purgeHook(step)
		}
		return ctx.Err()
	}
	mirrors, _ := s.Upstream.(MirrorCleaner)
	for _, repo := range t.Repos {
		for {
			if err := batch("objects"); err != nil {
				return OrgDeletion{}, err
			}
			n, err := s.Store.PurgeRepoObjects(ctx, orgID, repo, t.Repos)
			if err != nil {
				return OrgDeletion{}, err
			}
			if n == 0 {
				break
			}
		}
		if err := s.purgeRepoRows(ctx, orgID, repo, batch); err != nil {
			return OrgDeletion{}, err
		}
		if err := batch("repository"); err != nil {
			return OrgDeletion{}, err
		}
		if err := s.Store.PurgeRepoRow(ctx, orgID, repo); err != nil {
			return OrgDeletion{}, err
		}
		if mirrors != nil {
			if removed, err := mirrors.RemoveMirror(repo); err != nil {
				return OrgDeletion{}, fmt.Errorf("remove git mirror: %w", err)
			} else if removed {
				_ = s.Store.AddPurgeCounts(ctx, orgID, map[string]int64{"mirrors": 1})
			}
		}
	}
	if err := s.Store.SetPurgeStep(ctx, orgID, "blobs"); err != nil {
		return OrgDeletion{}, err
	}
	if err := s.sweepBlobs(ctx, orgID, t.Repos, batch); err != nil {
		return OrgDeletion{}, err
	}
	if err := s.Store.SetPurgeStep(ctx, orgID, "organization"); err != nil {
		return OrgDeletion{}, err
	}
	if err := batch("organization"); err != nil {
		return OrgDeletion{}, err
	}
	d, err := s.Store.PurgeOrgRows(ctx, orgID, t.Deletion.Handle, s.now())
	if err != nil {
		return OrgDeletion{}, err
	}
	_ = s.Store.PruneBlobDeletions(ctx, blobMarkKeep)
	metrics.OrgsPurged.Inc()
	return deletionFromStore(d), nil
}

// purgeRepoRows deletes a repository's large tables in batches, one table
// after the other (store.PurgeRepoStep).
func (s *Service) purgeRepoRows(ctx context.Context, orgID, repo uuid.UUID, batch func(string) error) error {
	for i := 0; ; i++ {
		for {
			if err := batch("rows"); err != nil {
				return err
			}
			n, ok, err := s.Store.PurgeRepoStep(ctx, orgID, repo, i)
			if err != nil || !ok {
				return err
			}
			if n < int64(store.PurgeBatch) {
				break
			}
		}
	}
}

// sweepBlobs deletes the marked blobs no repository records.
func (s *Service) sweepBlobs(ctx context.Context, orgID uuid.UUID, repos []uuid.UUID, batch func(string) error) error {
	if n, err := s.Store.PendingBackfillsOutside(ctx, repos); err != nil {
		return err
	} else if n > 0 {
		return errPurgeWaiting
	}
	var del blob.Deleter
	if s.Blobs != nil {
		del, _ = blob.AsDeleter(s.Blobs)
	}
	for {
		if err := batch("blobs"); err != nil {
			return err
		}
		_, handled, err := s.Store.SweepBlobs(ctx, &orgID, 200, func(ctx context.Context, h hash.Hash) error {
			if del == nil {
				return fmt.Errorf("the blob store %T cannot delete objects", s.Blobs)
			}
			return del.Delete(ctx, h)
		})
		if err != nil {
			return err
		}
		if handled == 0 {
			return nil
		}
	}
}

// SweepMirrors removes this server's git mirrors of repositories that no
// longer exist (purged here or by another server), and returns how many.
func (s *Service) SweepMirrors(ctx context.Context) (int, error) {
	m, ok := s.Upstream.(MirrorCleaner)
	if !ok {
		return 0, nil
	}
	ids, err := m.MirrorRepoIDs()
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	have, err := s.Store.ExistingRepoIDs(ctx, ids)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if have[id] {
			continue
		}
		if removed, err := m.RemoveMirror(id); err != nil {
			return n, err
		} else if removed {
			n++
		}
	}
	return n, nil
}

// blobPresent asks the blob store itself (no cache) whether it has an
// object, for store.RecordRepoObjects.
func (s *Service) blobPresent(ctx context.Context, h hash.Hash) (bool, error) {
	return blob.Fresh(s.Blobs).Has(ctx, h)
}

// errObjectsPurged is the error when objects a land or import made were
// deleted by an organization purge while it ran.
func errObjectsPurged(n int) error {
	return errf(CodeUnavailable, "%d objects were deleted by an organization purge while this ran (another organization had the same files); run it again", n)
}
