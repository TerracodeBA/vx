package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AuditFilter selects operation log entries for an export. Zero fields do
// not filter.
type AuditFilter struct {
	// Org limits to the organization's repositories and to the
	// organization-level entries about it (org.*, team.*, github.org.*
	// with the organization as target). OrgHandle must be its handle.
	Org       *uuid.UUID
	OrgHandle string
	Repo      *uuid.UUID
	Actor     *uuid.UUID
	Since     time.Time // at >= Since
	Until     time.Time // at < Until
	AfterID   int64     // id > AfterID, for paging
	Limit     int
	Desc      bool // newest first
	// Payload also reads before and after. Without it they stay nil and
	// never leave the database. With ServerPayloadOnly, only those of
	// entries without a repository are read.
	Payload           bool
	ServerPayloadOnly bool
}

// AuditEntry is one operation log entry with the names of what it refers
// to.
type AuditEntry struct {
	ID      int64
	At      time.Time
	Op      string
	Target  string
	RepoID  *uuid.UUID
	Repo    string // repository name, "" for server-level entries
	Org     string // the repository's organization, or the organization an org-level entry is about
	ActorID *uuid.UUID
	Actor   string // principal ref; "" for the server itself
	Before  json.RawMessage
	After   json.RawMessage
}

// orgLevelOps are the server-level operations that are about one
// organization, named in the target.
const orgLevelOps = `(o.op like 'org.%' or o.op like 'team.%' or o.op like 'github.org.%')`

// ListAudit returns entries matching f, oldest first (newest with Desc).
func (q Q) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	payload := `null::jsonb, null::jsonb`
	switch {
	case f.Payload && f.ServerPayloadOnly:
		payload = `case when o.repo_id is null then o.before end, case when o.repo_id is null then o.after end`
	case f.Payload:
		payload = `o.before, o.after`
	}
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Org != nil {
		h := arg(f.OrgHandle)
		where = append(where, `(r.org_id = `+arg(*f.Org)+` or (o.repo_id is null and `+orgLevelOps+
			` and (o.target = `+h+` or starts_with(o.target, `+h+` || '/'))))`)
	}
	if f.Repo != nil {
		where = append(where, `o.repo_id = `+arg(*f.Repo))
	}
	if f.Actor != nil {
		where = append(where, `o.actor_id = `+arg(*f.Actor))
	}
	if !f.Since.IsZero() {
		where = append(where, `o.at >= `+arg(f.Since))
	}
	if !f.Until.IsZero() {
		where = append(where, `o.at < `+arg(f.Until))
	}
	if f.AfterID > 0 {
		where = append(where, `o.id > `+arg(f.AfterID))
	}
	limit := f.Limit
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	sql := `select o.id, o.at, o.op, o.target, o.repo_id, coalesce(r.name, ''),
			coalesce(ro.handle, case when o.repo_id is null and ` + orgLevelOps + ` then split_part(o.target, '/', 1) else '' end),
			o.actor_id, p.kind, p.handle, ` + payload + `
		from op_log o
		left join repos r on r.id = o.repo_id
		left join orgs ro on ro.id = r.org_id
		left join principals p on p.id = o.actor_id`
	if len(where) > 0 {
		sql += ` where ` + strings.Join(where, ` and `)
	}
	order := ` order by o.id`
	if f.Desc {
		order += ` desc`
	}
	sql += order + ` limit ` + arg(limit)
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var kind, handle *string
		if err := rows.Scan(&e.ID, &e.At, &e.Op, &e.Target, &e.RepoID, &e.Repo, &e.Org, &e.ActorID, &kind, &handle, &e.Before, &e.After); err != nil {
			return nil, err
		}
		if kind != nil && handle != nil {
			e.Actor = Principal{Kind: PrincipalKind(*kind), Handle: *handle}.Ref()
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteOpsBefore deletes up to batch operation log entries older than t
// and returns how many it deleted.
func (q Q) DeleteOpsBefore(ctx context.Context, t time.Time, batch int) (int64, error) {
	tag, err := q.db.Exec(ctx, `delete from op_log where id in (select id from op_log where at < $1 order by id limit $2)`, t, batch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
