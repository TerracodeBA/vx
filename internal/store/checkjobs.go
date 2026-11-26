package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Job states.
const (
	JobQueued     = "queued"
	JobLeased     = "leased"
	JobDone       = "done"
	JobFailed     = "failed"
	JobSuperseded = "superseded"
)

// CheckJob is one check to run on one tree of an effort, by a runner that
// is not the landing agent (check_jobs). RepoName and EffortSlug are read
// from the job's repository and effort.
type CheckJob struct {
	ID              uuid.UUID
	RepoID          uuid.UUID
	EffortID        uuid.UUID
	Check           string
	Runner          string // agent:KIND, ci or human
	Reporter        string // the check's reporter rule when the job was made
	AllowSamePerson bool
	Trigger         string // land, ship or manual
	ChangeID        string
	Changeset       hash.Hash // zero before the change landed (a land gate's job)
	Intent          string
	BaseTree        hash.Hash
	Tree            hash.Hash
	AuthorPrincipal string // ref of who made the change
	AuthorPerson    string // ref of their person
	Inputs          []string
	Values          map[string]string
	State           string
	Attempts        int
	MaxAttempts     int
	Timeout         time.Duration
	LeaseOwner      *uuid.UUID
	LeaseExpiresAt  *time.Time
	LeasedAt        *time.Time
	Reason          string
	ResultID        string
	RequestedBy     *uuid.UUID
	SupersededBy    *uuid.UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time
	RepoName        string
	EffortSlug      string
}

// Pending reports whether the job still waits for a result.
func (j CheckJob) Pending() bool { return j.State == JobQueued || j.State == JobLeased }

const checkJobCols = `j.id, j.repo_id, j.effort_id, j.check_name, j.runner, j.reporter, j.allow_same_person, j.trigger,
	j.change_id, j.changeset_hash, j.intent, j.base_tree, j.tree, j.author_principal, j.author_person, j.inputs, j."values",
	j.state, j.attempts, j.max_attempts, j.timeout_seconds, j.lease_owner, j.lease_expires_at, j.leased_at,
	j.reason, j.result_id, j.requested_by, j.superseded_by, j.created_at, j.updated_at, r.name, e.slug`

const checkJobFrom = ` from check_jobs j join repos r on r.id = j.repo_id join efforts e on e.id = j.effort_id `

func scanCheckJob(row interface{ Scan(...any) error }) (CheckJob, error) {
	var j CheckJob
	var values []byte
	var timeout int
	err := row.Scan(&j.ID, &j.RepoID, &j.EffortID, &j.Check, &j.Runner, &j.Reporter, &j.AllowSamePerson, &j.Trigger,
		&j.ChangeID, &j.Changeset, &j.Intent, &j.BaseTree, &j.Tree, &j.AuthorPrincipal, &j.AuthorPerson, &j.Inputs, &values,
		&j.State, &j.Attempts, &j.MaxAttempts, &timeout, &j.LeaseOwner, &j.LeaseExpiresAt, &j.LeasedAt,
		&j.Reason, &j.ResultID, &j.RequestedBy, &j.SupersededBy, &j.CreatedAt, &j.UpdatedAt, &j.RepoName, &j.EffortSlug)
	if err != nil {
		return j, err
	}
	j.Timeout = time.Duration(timeout) * time.Second
	if j.Inputs == nil {
		j.Inputs = []string{}
	}
	j.Values = map[string]string{}
	if len(values) > 0 {
		if err := json.Unmarshal(values, &j.Values); err != nil {
			return j, fmt.Errorf("check job %s values: %w", j.ID, err)
		}
	}
	return j, nil
}

func (q Q) queryCheckJobs(ctx context.Context, sql string, args ...any) ([]CheckJob, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckJob
	for rows.Next() {
		j, err := scanCheckJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// EnqueueCheckJob stores a job and supersedes the pending jobs of the same
// check and effort for other trees. When a pending job for the same check
// and tree exists, no new one is made: j's values are merged into it (new
// values win) and it comes back with created false. For triggers other than
// manual, any earlier job for the tree that was not superseded comes back
// unchanged instead.
func (q Q) EnqueueCheckJob(ctx context.Context, j CheckJob, now time.Time) (CheckJob, bool, error) {
	if j.ID == uuid.Nil {
		j.ID = NewID()
	}
	if j.Values == nil {
		j.Values = map[string]string{}
	}
	if j.Inputs == nil {
		j.Inputs = []string{}
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = 3
	}
	if j.Timeout <= 0 {
		j.Timeout = 30 * time.Minute
	}
	if j.Trigger != "manual" {
		// Land, ship and pipelines ask again for a tree they asked for
		// before: the job made then (pending, done or failed) stands. Only
		// a request runs a check on the same tree again.
		ex, err := scanCheckJob(q.db.QueryRow(ctx, `select `+checkJobCols+checkJobFrom+`
			where j.effort_id = $1 and j.check_name = $2 and j.tree = $3 and j.state <> 'superseded'
			order by j.created_at desc limit 1`, j.EffortID, j.Check, j.Tree))
		if err == nil {
			return ex, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return CheckJob{}, false, err
		}
	}
	values, err := json.Marshal(j.Values)
	if err != nil {
		return CheckJob{}, false, err
	}
	var id uuid.UUID
	var inserted bool
	err = q.db.QueryRow(ctx, `insert into check_jobs (id, repo_id, effort_id, check_name, runner, reporter, allow_same_person,
			trigger, change_id, changeset_hash, base_tree, tree, author_principal, author_person, inputs, "values",
			max_attempts, timeout_seconds, requested_by, created_at, updated_at, intent)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $20, $21)
		on conflict (effort_id, check_name, tree) where state in ('queued', 'leased')
		do update set "values" = check_jobs."values" || excluded."values", updated_at = excluded.updated_at
		returning id, (xmax = 0)`,
		j.ID, j.RepoID, j.EffortID, j.Check, j.Runner, j.Reporter, j.AllowSamePerson, j.Trigger, j.ChangeID,
		j.Changeset, j.BaseTree, j.Tree, j.AuthorPrincipal, j.AuthorPerson, j.Inputs, values,
		j.MaxAttempts, int(j.Timeout/time.Second), j.RequestedBy, now, j.Intent).Scan(&id, &inserted)
	if err != nil {
		return CheckJob{}, false, err
	}
	if _, err := q.db.Exec(ctx, `update check_jobs set state = 'superseded', superseded_by = $4, updated_at = $5,
			reason = $6, lease_owner = null, lease_expires_at = null
		where effort_id = $1 and check_name = $2 and tree <> $3 and state in ('queued', 'leased')`,
		j.EffortID, j.Check, j.Tree, id, now, "a newer tree of the effort needs this check: job "+id.String()[:8]); err != nil {
		return CheckJob{}, false, err
	}
	out, err := q.CheckJobByID(ctx, id)
	return out, inserted, err
}

// CheckJobByID returns one job.
func (q Q) CheckJobByID(ctx context.Context, id uuid.UUID) (CheckJob, error) {
	j, err := scanCheckJob(q.db.QueryRow(ctx, `select `+checkJobCols+checkJobFrom+`where j.id = $1`, id))
	return j, notFound(err, "check job "+id.String())
}

// CheckJobsByPrefix returns the jobs of the given repositories whose id
// starts with prefix (hex digits of the id, dashes ignored).
func (q Q) CheckJobsByPrefix(ctx context.Context, repoIDs []uuid.UUID, prefix string) ([]CheckJob, error) {
	return q.queryCheckJobs(ctx, `select `+checkJobCols+checkJobFrom+`
		where j.repo_id = any($1) and replace(j.id::text, '-', '') like $2 || '%' order by j.created_at limit 5`, repoIDs, prefix)
}

// ReapCheckJobs puts the jobs whose lease expired before now back in the
// queue with the attempt counted, and fails those that used up their
// attempts. It returns how many it requeued and failed. Several servers may
// run it at once: each row is updated by one of them.
func (q Q) ReapCheckJobs(ctx context.Context, now time.Time) (requeued, failed int, err error) {
	rows, err := q.db.Query(ctx, `update check_jobs j set
			attempts = j.attempts + 1,
			state = case when j.attempts + 1 >= j.max_attempts then 'failed' else 'queued' end,
			reason = case when j.attempts + 1 >= j.max_attempts
				then 'the lease expired ' || (j.attempts + 1)::text || ' times without a result; last taken by ' || coalesce(p.handle, 'a deleted runner')
				else 'the lease of ' || coalesce(p.handle, 'a deleted runner') || ' expired without a result (attempt ' || (j.attempts + 1)::text || ' of ' || j.max_attempts::text || ')' end,
			lease_owner = null, lease_expires_at = null, updated_at = $1
		from check_jobs j0 left join principals p on p.id = j0.lease_owner
		where j0.id = j.id and j.state = 'leased' and j.lease_expires_at < $1
		returning j.state`, now)
	if err != nil {
		return 0, 0, err
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, s := range states {
		if s == JobFailed {
			failed++
		} else {
			requeued++
		}
	}
	return requeued, failed, err
}

// QueuedCheckJobs locks up to limit of the oldest queued jobs of the given
// repositories and runners, skipping rows another transaction holds (FOR
// UPDATE SKIP LOCKED), so servers taking jobs at once get different ones.
// Use it in a transaction and lease one of the rows before committing.
func (q Q) QueuedCheckJobs(ctx context.Context, repoIDs []uuid.UUID, effortID *uuid.UUID, runners []string, limit int) ([]CheckJob, error) {
	rows, err := q.db.Query(ctx, `select j.id from check_jobs j
		where j.state = 'queued' and j.repo_id = any($1) and j.runner = any($2) and ($3::uuid is null or j.effort_id = $3)
		order by j.created_at, j.id limit $4 for update skip locked`, repoIDs, runners, effortID, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return q.queryCheckJobs(ctx, `select `+checkJobCols+checkJobFrom+`where j.id = any($1) order by j.created_at, j.id`, ids)
}

// LeaseCheckJob gives a queued job to owner until expires.
func (q Q) LeaseCheckJob(ctx context.Context, id, owner uuid.UUID, now, expires time.Time) (CheckJob, error) {
	tag, err := q.db.Exec(ctx, `update check_jobs set state = 'leased', lease_owner = $2, leased_at = $3, lease_expires_at = $4,
		updated_at = $3 where id = $1 and state = 'queued'`, id, owner, now, expires)
	if err != nil {
		return CheckJob{}, err
	}
	if tag.RowsAffected() == 0 {
		return CheckJob{}, ErrLeaseLost
	}
	return q.CheckJobByID(ctx, id)
}

// ErrLeaseLost is returned when leasing a job that is no longer queued, or
// renewing or releasing one whose lease the caller no longer holds.
var ErrLeaseLost = errors.New("lease lost")

// RenewCheckJob moves the end of owner's lease on a job to expires.
func (q Q) RenewCheckJob(ctx context.Context, id, owner uuid.UUID, now, expires time.Time) (CheckJob, error) {
	tag, err := q.db.Exec(ctx, `update check_jobs set lease_expires_at = $4, updated_at = $3
		where id = $1 and state = 'leased' and lease_owner = $2`, id, owner, now, expires)
	if err != nil {
		return CheckJob{}, err
	}
	if tag.RowsAffected() == 0 {
		return CheckJob{}, ErrLeaseLost
	}
	return q.CheckJobByID(ctx, id)
}

// ReleaseCheckJob puts owner's leased job back in the queue without
// counting an attempt.
func (q Q) ReleaseCheckJob(ctx context.Context, id, owner uuid.UUID, now time.Time, reason string) (CheckJob, error) {
	tag, err := q.db.Exec(ctx, `update check_jobs set state = 'queued', lease_owner = null, lease_expires_at = null,
		reason = $4, updated_at = $3 where id = $1 and state = 'leased' and lease_owner = $2`, id, owner, now, reason)
	if err != nil {
		return CheckJob{}, err
	}
	if tag.RowsAffected() == 0 {
		return CheckJob{}, ErrLeaseLost
	}
	return q.CheckJobByID(ctx, id)
}

// FinishCheckJobs marks the pending jobs of a check on a tree of an effort
// done with the result that finished them, and returns them.
func (q Q) FinishCheckJobs(ctx context.Context, effortID uuid.UUID, check string, tree hash.Hash, resultID string, now time.Time) ([]CheckJob, error) {
	rows, err := q.db.Query(ctx, `update check_jobs set state = 'done', result_id = $4, reason = '', lease_owner = null,
		lease_expires_at = null, updated_at = $5 where effort_id = $1 and check_name = $2 and tree = $3 and state in ('queued', 'leased')
		returning id`, effortID, check, tree, resultID, now)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return q.queryCheckJobs(ctx, `select `+checkJobCols+checkJobFrom+`where j.id = any($1) order by j.created_at`, ids)
}

// CheckJobFilter selects jobs for ListCheckJobs. Empty fields do not
// filter; RepoIDs must not be empty.
type CheckJobFilter struct {
	RepoIDs  []uuid.UUID
	EffortID *uuid.UUID
	States   []string
	Runners  []string
	Limit    int
}

// ListCheckJobs returns jobs, pending ones first, then newest first.
func (q Q) ListCheckJobs(ctx context.Context, f CheckJobFilter) ([]CheckJob, error) {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	return q.queryCheckJobs(ctx, `select `+checkJobCols+checkJobFrom+`
		where j.repo_id = any($1) and ($2::uuid is null or j.effort_id = $2)
		and (cardinality($3::text[]) = 0 or j.state = any($3)) and (cardinality($4::text[]) = 0 or j.runner = any($4))
		and e.state = 'open'
		order by (j.state in ('queued', 'leased')) desc, j.created_at desc limit $5`,
		f.RepoIDs, f.EffortID, nonNil(f.States), nonNil(f.Runners), f.Limit)
}

// AgentRunnerKinds returns the runner kinds an agent declared; none when it
// declared none.
func (q Q) AgentRunnerKinds(ctx context.Context, agentID uuid.UUID) ([]string, error) {
	var kinds []string
	err := q.db.QueryRow(ctx, `select kinds from agent_runner_kinds where agent_id = $1`, agentID).Scan(&kinds)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{}, nil
	}
	if kinds == nil {
		kinds = []string{}
	}
	return kinds, err
}

// SetAgentRunnerKinds replaces the runner kinds of an agent.
func (q Q) SetAgentRunnerKinds(ctx context.Context, agentID uuid.UUID, kinds []string, by uuid.UUID, now time.Time) error {
	_, err := q.db.Exec(ctx, `insert into agent_runner_kinds (agent_id, kinds, updated_by, updated_at) values ($1, $2, $3, $4)
		on conflict (agent_id) do update set kinds = excluded.kinds, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		agentID, nonNil(kinds), by, now)
	return err
}

// CIToken is a CI token of a repository (ci_tokens): a service principal
// that may only report results on it and read its jobs' inputs.
type CIToken struct {
	ID          uuid.UUID
	RepoID      uuid.UUID
	Name        string
	PrincipalID uuid.UUID
	TokenID     uuid.UUID
	Checks      []string // empty: every check
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
	RevokedAt   *time.Time
	RepoName    string
	Handle      string // the service principal's handle
}

const ciTokenCols = `c.id, c.repo_id, c.name, c.principal_id, c.token_id, c.checks, c.created_by, c.created_at, c.revoked_at, r.name, p.handle`
const ciTokenFrom = ` from ci_tokens c join repos r on r.id = c.repo_id join principals p on p.id = c.principal_id `

func scanCIToken(row interface{ Scan(...any) error }) (CIToken, error) {
	var t CIToken
	err := row.Scan(&t.ID, &t.RepoID, &t.Name, &t.PrincipalID, &t.TokenID, &t.Checks, &t.CreatedBy, &t.CreatedAt, &t.RevokedAt, &t.RepoName, &t.Handle)
	if t.Checks == nil {
		t.Checks = []string{}
	}
	return t, err
}

// InsertCIToken stores a CI token record.
func (q Q) InsertCIToken(ctx context.Context, t CIToken) (CIToken, error) {
	if t.ID == uuid.Nil {
		t.ID = NewID()
	}
	_, err := q.db.Exec(ctx, `insert into ci_tokens (id, repo_id, name, principal_id, token_id, checks, created_by, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`, t.ID, t.RepoID, t.Name, t.PrincipalID, t.TokenID, nonNil(t.Checks), t.CreatedBy, t.CreatedAt)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return CIToken{}, fmt.Errorf("CI token %q: %w", t.Name, ErrExists)
		}
		return CIToken{}, err
	}
	return scanCIToken(q.db.QueryRow(ctx, `select `+ciTokenCols+ciTokenFrom+`where c.id = $1`, t.ID))
}

// CITokenOf returns the active CI token whose service principal is
// principalID. Revoked tokens and repositories of deleted organizations
// are not found.
func (q Q) CITokenOf(ctx context.Context, principalID uuid.UUID) (CIToken, error) {
	t, err := scanCIToken(q.db.QueryRow(ctx, `select `+ciTokenCols+ciTokenFrom+`
		join orgs o on o.id = r.org_id and o.deleted_at is null where c.principal_id = $1 and c.revoked_at is null`, principalID))
	return t, notFound(err, "CI token")
}

// ListCITokens returns a repository's CI tokens, active ones first.
func (q Q) ListCITokens(ctx context.Context, repoID uuid.UUID, withRevoked bool) ([]CIToken, error) {
	rows, err := q.db.Query(ctx, `select `+ciTokenCols+ciTokenFrom+`where c.repo_id = $1 and ($2 or c.revoked_at is null)
		order by (c.revoked_at is null) desc, c.created_at`, repoID, withRevoked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CIToken
	for rows.Next() {
		t, err := scanCIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeCIToken revokes a repository's active CI token by name, and its
// token.
func (q Q) RevokeCIToken(ctx context.Context, repoID uuid.UUID, name string, now time.Time) (CIToken, error) {
	t, err := scanCIToken(q.db.QueryRow(ctx, `select `+ciTokenCols+ciTokenFrom+`where c.repo_id = $1 and c.name = $2 and c.revoked_at is null`, repoID, name))
	if err != nil {
		return CIToken{}, notFound(err, fmt.Sprintf("CI token %q", name))
	}
	if _, err := q.db.Exec(ctx, `update ci_tokens set revoked_at = $2 where id = $1`, t.ID, now); err != nil {
		return CIToken{}, err
	}
	if _, err := q.db.Exec(ctx, `update tokens set revoked_at = $2 where principal_id = $1 and revoked_at is null`, t.PrincipalID, now); err != nil {
		return CIToken{}, err
	}
	t.RevokedAt = &now
	return t, nil
}
