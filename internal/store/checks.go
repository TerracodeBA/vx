package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Check result statuses.
const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckNeutral = "neutral"
	CheckPending = "pending"
)

// Where a check result came from.
const (
	CheckSourceReport = "report" // vx check report (agents, CI, people)
	CheckSourceLand   = "land"   // a vx land --run command
	CheckSourceReview = "review" // a person approved a review under a human [[policy]]
)

// CheckResult is one result of a check, bound to a change and the exact
// tree it describes (migration 036).
type CheckResult struct {
	ID         uuid.UUID
	RepoID     uuid.UUID
	EffortID   *uuid.UUID
	ChangeID   string
	Tree       hash.Hash
	CheckName  string // "" for a vx land --run command no check names
	Status     string
	Summary    string
	LogHash    hash.Hash // zero: no log
	LogSize    int64
	Data       json.RawMessage // nil: no data
	DataName   string
	ReporterID *uuid.UUID
	Reporter   string // principal ref
	OnBehalfOf string
	Model      string
	CITokenID  *uuid.UUID
	How        string // the command of a run check
	Source     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	Artifacts  []CheckArtifact
}

// CheckArtifact is a file attached to a check result: a blob of the
// repository.
type CheckArtifact struct {
	Name        string
	ContentType string
	Hash        hash.Hash
	Size        int64
}

const checkResultCols = `id, repo_id, effort_id, change_id, tree, check_name, status, summary, log_hash, log_size, data, data_name,
	reporter_id, reporter, on_behalf_of, model, ci_token_id, how, source, started_at, finished_at, created_at`

func scanCheckResult(row interface{ Scan(...any) error }) (CheckResult, error) {
	var r CheckResult
	var data []byte
	err := row.Scan(&r.ID, &r.RepoID, &r.EffortID, &r.ChangeID, &r.Tree, &r.CheckName, &r.Status, &r.Summary, &r.LogHash, &r.LogSize,
		&data, &r.DataName, &r.ReporterID, &r.Reporter, &r.OnBehalfOf, &r.Model, &r.CITokenID, &r.How, &r.Source,
		&r.StartedAt, &r.FinishedAt, &r.CreatedAt)
	if len(data) > 0 {
		r.Data = json.RawMessage(data)
	}
	return r, err
}

// InsertCheckResult stores r and its artifacts and returns it as stored
// (id and created_at set). Run it in a transaction so the artifacts are
// stored with it.
func (q Q) InsertCheckResult(ctx context.Context, r CheckResult) (CheckResult, error) {
	var data any
	if len(r.Data) > 0 {
		data = []byte(r.Data)
	}
	out, err := scanCheckResult(q.db.QueryRow(ctx, `insert into check_results (repo_id, effort_id, change_id, tree, check_name,
		status, summary, log_hash, log_size, data, data_name, reporter_id, reporter, on_behalf_of, model, ci_token_id, how, source,
		started_at, finished_at) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		returning `+checkResultCols,
		r.RepoID, r.EffortID, r.ChangeID, r.Tree, r.CheckName, r.Status, r.Summary, r.LogHash, r.LogSize, data, r.DataName,
		r.ReporterID, r.Reporter, r.OnBehalfOf, r.Model, r.CITokenID, r.How, r.Source, r.StartedAt, r.FinishedAt))
	if err != nil {
		return CheckResult{}, fmt.Errorf("insert check result: %w", err)
	}
	for i, a := range r.Artifacts {
		if _, err := q.db.Exec(ctx, `insert into check_artifacts (result_id, position, name, content_type, hash, size)
			values ($1, $2, $3, $4, $5, $6)`, out.ID, i, a.Name, a.ContentType, a.Hash, a.Size); err != nil {
			return CheckResult{}, fmt.Errorf("insert check artifact %s: %w", a.Name, err)
		}
	}
	out.Artifacts = r.Artifacts
	return out, nil
}

// CheckResultFilter selects check results of one repository. Zero fields
// do not filter.
type CheckResultFilter struct {
	RepoID   uuid.UUID
	EffortID uuid.UUID
	ChangeID string
	Tree     hash.Hash
	Trees    []hash.Hash // any of these trees
	Check    string
	Limit    int
}

// ListCheckResults returns check results, newest first, with their
// artifacts.
func (q Q) ListCheckResults(ctx context.Context, f CheckResultFilter) ([]CheckResult, error) {
	if f.Limit <= 0 {
		f.Limit = 500
	}
	var effort *uuid.UUID
	if f.EffortID != uuid.Nil {
		effort = &f.EffortID
	}
	var change, check *string
	if f.ChangeID != "" {
		change = &f.ChangeID
	}
	if f.Check != "" {
		check = &f.Check
	}
	trees := f.Trees
	if !f.Tree.IsZero() {
		trees = append(trees, f.Tree)
	}
	var treeBytes [][]byte
	for _, t := range trees {
		treeBytes = append(treeBytes, t.Bytes())
	}
	rows, err := q.db.Query(ctx, `select `+checkResultCols+` from check_results
		where repo_id = $1 and ($2::uuid is null or effort_id = $2) and ($3::text is null or change_id = $3)
		and ($4::bytea[] is null or tree = any($4)) and ($5::text is null or check_name = $5)
		order by created_at desc, id limit $6`, f.RepoID, effort, change, treeBytes, check, f.Limit)
	if err != nil {
		return nil, err
	}
	var out []CheckResult
	for rows.Next() {
		r, err := scanCheckResult(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, q.loadCheckArtifacts(ctx, out)
}

func (q Q) loadCheckArtifacts(ctx context.Context, rs []CheckResult) error {
	if len(rs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(rs))
	at := map[uuid.UUID]int{}
	for i, r := range rs {
		ids[i] = r.ID
		at[r.ID] = i
	}
	rows, err := q.db.Query(ctx, `select result_id, name, content_type, hash, size from check_artifacts
		where result_id = any($1) order by result_id, position`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var a CheckArtifact
		if err := rows.Scan(&id, &a.Name, &a.ContentType, &a.Hash, &a.Size); err != nil {
			return err
		}
		rs[at[id]].Artifacts = append(rs[at[id]].Artifacts, a)
	}
	return rows.Err()
}

// GetCheckResult loads one check result with its artifacts.
func (q Q) GetCheckResult(ctx context.Context, id uuid.UUID) (CheckResult, error) {
	r, err := scanCheckResult(q.db.QueryRow(ctx, `select `+checkResultCols+` from check_results where id = $1`, id))
	if err != nil {
		return CheckResult{}, notFound(err, "check result")
	}
	rs := []CheckResult{r}
	if err := q.loadCheckArtifacts(ctx, rs); err != nil {
		return CheckResult{}, err
	}
	return rs[0], nil
}

// CheckResultByPrefix resolves a check result id or a prefix of at least
// 8 hex characters among the results of the given repositories (nil: any).
func (q Q) CheckResultByPrefix(ctx context.Context, prefix string, repos []uuid.UUID) (CheckResult, error) {
	if id, err := uuid.Parse(prefix); err == nil {
		return q.GetCheckResult(ctx, id)
	}
	rows, err := q.db.Query(ctx, `select id from check_results
		where id::text like $1 || '%' and ($2::uuid[] is null or repo_id = any($2)) order by created_at desc limit 2`, prefix, repos)
	if err != nil {
		return CheckResult{}, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return CheckResult{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CheckResult{}, err
	}
	switch len(ids) {
	case 0:
		return CheckResult{}, fmt.Errorf("check result %s: %w", prefix, ErrNotFound)
	case 1:
		return q.GetCheckResult(ctx, ids[0])
	}
	return CheckResult{}, fmt.Errorf("check result %s matches more than one result; give more of the id: %w", prefix, ErrAmbiguous)
}
