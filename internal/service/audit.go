package service

// Audit log export (#10): the operation log as JSONL or CSV, filtered by
// organization, repository, actor and time.
//
// Who sees what (the superadmin rule of #33: operation metadata yes,
// repository content no):
//   - superadmins export the whole server, metadata only for repository
//     entries: id, time, op, organization, repository, actor and target;
//     before/after (intents, titles, paths) stay in the database. They get
//     before/after of server-level entries (people, invites, tokens).
//   - owners and admins of an organization export that organization only:
//     its organization-level entries and the entries of its repositories,
//     with before/after for the repositories they are admin of.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/store"
)

// AuditSettings configure the operation log.
type AuditSettings struct {
	// Retention deletes entries older than this (VX_AUDIT_RETENTION); 0
	// keeps them forever. At least MinAuditRetention.
	Retention time.Duration
}

// MinAuditRetention is the shortest retention: the log also answers "who
// did this" for recent work.
const MinAuditRetention = 7 * 24 * time.Hour

// AuditQuery filters an export. Empty fields do not filter.
type AuditQuery struct {
	Org   string // organization handle
	Repo  string // repository name
	Actor string // principal ref or handle: user:alice, alice, agent:alice/claude
	Since time.Time
	Until time.Time
	// Newest, when set, exports only the newest Newest entries, newest
	// first (the web page's preview).
	Newest int
}

// AuditRow is one exported entry.
type AuditRow struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Op     string          `json:"op"`
	Org    string          `json:"org,omitempty"`
	Repo   string          `json:"repo,omitempty"`
	Actor  string          `json:"actor,omitempty"`
	Target string          `json:"target"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// AuditColumns are the CSV columns.
var AuditColumns = []string{"id", "at", "op", "org", "repo", "actor", "target", "before", "after"}

// auditScope is what one caller may export.
type auditScope struct {
	filter     store.AuditFilter
	superadmin bool
	adminRepos map[uuid.UUID]bool // repositories whose payloads the caller sees
}

func (s *Service) auditScope(ctx context.Context, actor Actor, q AuditQuery) (auditScope, error) {
	sc := auditScope{superadmin: actor.Kind == store.KindHuman && s.IsSuperadmin(ctx, actor), adminRepos: map[uuid.UUID]bool{}}
	f := store.AuditFilter{Since: q.Since, Until: q.Until, Payload: true}
	if org := strings.TrimSpace(q.Org); org != "" {
		a, err := s.loadOrg(ctx, nil, actor, org)
		if err != nil {
			return sc, err
		}
		if !sc.superadmin && a.role != OrgOwner && a.role != OrgAdmin {
			return sc, errf(CodePermission, "only owners and admins of %s export its audit log; ask one of them (%s)", a.org.Handle, s.orgOwnerRefs(ctx, a.org.ID))
		}
		f.Org, f.OrgHandle = &a.org.ID, a.org.Handle
		if a.role == OrgOwner || a.role == OrgAdmin {
			repos, roles, err := s.ReadableRepos(ctx, actor, &a.org.ID)
			if err != nil {
				return sc, err
			}
			for i, r := range repos {
				if roles[i] == RoleAdmin {
					sc.adminRepos[r.ID] = true
				}
			}
		}
	} else if !sc.superadmin {
		return sc, errf(CodePermission, "only superadmins export the whole server's audit log; export one organization you own or admin: vx admin audit --org ORG")
	}
	if name := strings.TrimSpace(q.Repo); name != "" {
		r, err := s.Store.RepoByName(ctx, name)
		if err != nil || (f.Org != nil && r.OrgID != *f.Org) {
			return sc, errf(CodeNotFound, "repository %s not found", name)
		}
		if f.Org == nil && !sc.superadmin {
			return sc, errf(CodeNotFound, "repository %s not found", name)
		}
		f.Repo = &r.ID
	}
	if ref := strings.TrimSpace(q.Actor); ref != "" {
		handle := ref
		if i := strings.Index(ref, ":"); i >= 0 {
			handle = ref[i+1:]
		}
		p, err := s.Store.PrincipalByHandle(ctx, handle)
		if err != nil {
			return sc, errf(CodeNotFound, "no one called %s on this server", ref)
		}
		f.Actor = &p.ID
	}
	// Repository payloads leave the database only for repositories the
	// caller administers.
	f.ServerPayloadOnly = len(sc.adminRepos) == 0
	sc.filter = f
	return sc, nil
}

// ExportAudit calls emit for every entry the caller may see that matches
// q, oldest first.
func (s *Service) ExportAudit(ctx context.Context, actor Actor, q AuditQuery, emit func(AuditRow) error) error {
	sc, err := s.auditScope(ctx, actor, q)
	if err != nil {
		return err
	}
	f := sc.filter
	f.Limit = 1000
	if q.Newest > 0 {
		f.Desc, f.Limit = true, q.Newest
	}
	for {
		entries, err := s.Store.ListAudit(ctx, f)
		if err != nil {
			return classify(err)
		}
		for _, e := range entries {
			row := AuditRow{ID: e.ID, At: e.At.UTC(), Op: e.Op, Org: e.Org, Repo: e.Repo, Actor: e.Actor, Target: e.Target}
			if sc.payload(e) {
				row.Before, row.After = e.Before, e.After
			}
			if err := emit(row); err != nil {
				return err
			}
		}
		if len(entries) < f.Limit || f.Desc {
			return nil
		}
		f.AfterID = entries[len(entries)-1].ID
	}
}

// payload reports whether the caller sees e's before and after.
func (sc auditScope) payload(e store.AuditEntry) bool {
	if e.RepoID != nil {
		return sc.adminRepos[*e.RepoID]
	}
	// Server-level entries: superadmins see all; organization admins
	// only reach the ones about their organization.
	return sc.superadmin || sc.filter.Org != nil
}

// WriteAudit writes an export in format "jsonl" (one JSON object per line)
// or "csv" (a header row, then AuditColumns).
func (s *Service) WriteAudit(ctx context.Context, actor Actor, q AuditQuery, format string, w io.Writer) error {
	switch format {
	case "", "jsonl", "json":
		enc := json.NewEncoder(w)
		return s.ExportAudit(ctx, actor, q, func(r AuditRow) error { return enc.Encode(r) })
	case "csv":
		cw := csv.NewWriter(w)
		if err := cw.Write(AuditColumns); err != nil {
			return err
		}
		err := s.ExportAudit(ctx, actor, q, func(r AuditRow) error {
			return cw.Write([]string{strconv.FormatInt(r.ID, 10), r.At.Format(time.RFC3339Nano), r.Op, r.Org, r.Repo, r.Actor, r.Target, string(r.Before), string(r.After)})
		})
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}
		return err
	}
	return errf(CodeInvalid, "audit format %q: use jsonl or csv", format)
}

// CheckAuditQuery validates q for actor without exporting, so a web page
// or download can fail with a message before it starts writing.
func (s *Service) CheckAuditQuery(ctx context.Context, actor Actor, q AuditQuery) error {
	_, err := s.auditScope(ctx, actor, q)
	return err
}

// PruneAudit deletes operation log entries older than the retention and
// returns how many. Without a retention it does nothing.
func (s *Service) PruneAudit(ctx context.Context) (int64, error) {
	ret := s.Audit.Retention
	if ret <= 0 {
		return 0, nil
	}
	ret = max(ret, MinAuditRetention)
	cut := s.Now().Add(-ret)
	var total int64
	for {
		n, err := s.Store.DeleteOpsBefore(ctx, cut, 5000)
		total += n
		metrics.AuditPruned.Add(float64(n))
		if err != nil || n < 5000 {
			return total, err
		}
	}
}

// PruneAuditLoop prunes once an hour until ctx ends.
func (s *Service) PruneAuditLoop(ctx context.Context, log *slog.Logger) {
	if s.Audit.Retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.PruneAudit(ctx); err != nil && ctx.Err() == nil {
			log.Warn("audit retention: could not delete old operation log entries", "err", err)
		} else if n > 0 {
			log.Info("audit retention: deleted old operation log entries", "count", n, "retention", s.Audit.Retention.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ParseRetention reads a retention like 90d, 2160h or 1y; "" and "0" keep
// the log forever.
func ParseRetention(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "0" || v == "forever" || v == "off" {
		return 0, nil
	}
	mult := time.Duration(0)
	switch {
	case strings.HasSuffix(v, "d"):
		mult = 24 * time.Hour
	case strings.HasSuffix(v, "w"):
		mult = 7 * 24 * time.Hour
	case strings.HasSuffix(v, "y"):
		mult = 365 * 24 * time.Hour
	}
	var d time.Duration
	if mult > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(v[:len(v)-1]))
		if err != nil || n < 0 {
			return 0, errf(CodeInvalid, "retention %q: write days, weeks or years like 90d, 12w or 1y", v)
		}
		d = time.Duration(n) * mult
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, errf(CodeInvalid, "retention %q: write days, weeks or years like 90d, 12w or 1y", v)
		}
	}
	if d > 0 && d < MinAuditRetention {
		return 0, errf(CodeInvalid, "retention %q is shorter than the minimum of 7 days", v)
	}
	return d, nil
}
