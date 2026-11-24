package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Shipment is an effort's changes pushed as a branch, usually with a PR.
type Shipment struct {
	ID          uuid.UUID
	EffortID    uuid.UUID
	Title       string
	Branch      string
	Target      string
	BaseGitSHA  string
	HeadGitSHA  string
	ProviderRef string
	URL         string
	State       string
	// Split is how the parts of an effort shipped as several pull
	// requests relate: "independent" or "stacked"; empty for one PR.
	Split string
	// Part is the 1-based position among the effort's parts; 0 for one PR.
	Part      int
	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	Changes   []ShipmentChange
	// BaseTree is the target's tree the commits were made on; zero for
	// shipments made before v0.4.
	BaseTree hash.Hash
}

// ShipmentChange is one changeset in a shipment and the commit made for it.
type ShipmentChange struct {
	Position  int
	Changeset hash.Hash
	ChangeID  string
	GitSHA    string
	// Tree is the tree of the pushed commit; zero for shipments made
	// before v0.4. Review comments on lines map to changes through it.
	Tree hash.Hash
}

const shipmentCols = `id, effort_id, title, branch, target, base_git_sha, head_git_sha, provider_ref, url, state, split, part, created_by, created_at, updated_at`

func scanShipment(row interface{ Scan(...any) error }) (Shipment, error) {
	var s Shipment
	err := row.Scan(&s.ID, &s.EffortID, &s.Title, &s.Branch, &s.Target, &s.BaseGitSHA, &s.HeadGitSHA,
		&s.ProviderRef, &s.URL, &s.State, &s.Split, &s.Part, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// SaveShipment inserts a shipment or updates the one for the same effort
// and branch, replacing its change list.
func (q Q) SaveShipment(ctx context.Context, s Shipment) (Shipment, error) {
	row := q.db.QueryRow(ctx, `insert into shipments
		(effort_id, title, branch, target, base_git_sha, head_git_sha, provider_ref, url, state, split, part, created_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (effort_id, branch) do update set
			title = excluded.title, target = excluded.target, base_git_sha = excluded.base_git_sha,
			head_git_sha = excluded.head_git_sha, provider_ref = excluded.provider_ref, url = excluded.url,
			state = excluded.state, split = excluded.split, part = excluded.part, updated_at = now()
		returning `+shipmentCols,
		s.EffortID, s.Title, s.Branch, s.Target, s.BaseGitSHA, s.HeadGitSHA, s.ProviderRef, s.URL, s.State, s.Split, s.Part, s.CreatedBy)
	out, err := scanShipment(row)
	if err != nil {
		return Shipment{}, err
	}
	if _, err := q.db.Exec(ctx, `delete from shipment_changes where shipment_id = $1`, out.ID); err != nil {
		return Shipment{}, err
	}
	for _, c := range s.Changes {
		if _, err := q.db.Exec(ctx, `insert into shipment_changes (shipment_id, position, changeset_hash, change_id, git_sha, tree)
			values ($1, $2, $3, $4, $5, $6)`, out.ID, c.Position, c.Changeset, c.ChangeID, c.GitSHA, c.Tree); err != nil {
			return Shipment{}, err
		}
	}
	if _, err := q.db.Exec(ctx, `update shipments set base_tree = $2 where id = $1`, out.ID, s.BaseTree); err != nil {
		return Shipment{}, err
	}
	out.Changes, out.BaseTree = s.Changes, s.BaseTree
	return out, nil
}

// ListShipments returns an effort's shipments with their changes: oldest
// first, the parts of a split in part order.
func (q Q) ListShipments(ctx context.Context, effortID uuid.UUID) ([]Shipment, error) {
	rows, err := q.db.Query(ctx, `select `+shipmentCols+` from shipments where effort_id = $1 order by created_at, part`, effortID)
	if err != nil {
		return nil, err
	}
	return q.shipmentsWithChanges(ctx, rows)
}

// ShipmentByBranch returns the shipment of an effort in repoID pushed to
// branch, with its changes.
func (q Q) ShipmentByBranch(ctx context.Context, repoID uuid.UUID, branch string) (Shipment, error) {
	rows, err := q.db.Query(ctx, `select `+prefixCols("s.", shipmentCols)+` from shipments s
		join efforts e on e.id = s.effort_id where e.repo_id = $1 and s.branch = $2
		order by s.updated_at desc limit 1`, repoID, branch)
	if err != nil {
		return Shipment{}, err
	}
	out, err := q.shipmentsWithChanges(ctx, rows)
	if err != nil {
		return Shipment{}, err
	}
	if len(out) == 0 {
		return Shipment{}, ErrNotFound
	}
	return out[0], nil
}

// SetShipmentState sets one shipment's state (a part of a split merged on
// its own, or left out of a newer split).
func (q Q) SetShipmentState(ctx context.Context, id uuid.UUID, state string) error {
	_, err := q.db.Exec(ctx, `update shipments set state = $1, updated_at = now() where id = $2`, state, id)
	return err
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = prefix + c
	}
	return strings.Join(parts, ", ")
}

func (q Q) shipmentsWithChanges(ctx context.Context, rows pgx.Rows) ([]Shipment, error) {
	var out []Shipment
	for rows.Next() {
		s, err := scanShipment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := q.db.QueryRow(ctx, `select base_tree from shipments where id = $1`, out[i].ID).Scan(&out[i].BaseTree); err != nil {
			return nil, err
		}
		crow, err := q.db.Query(ctx, `select position, changeset_hash, change_id, git_sha, tree from shipment_changes
			where shipment_id = $1 order by position`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for crow.Next() {
			var c ShipmentChange
			if err := crow.Scan(&c.Position, &c.Changeset, &c.ChangeID, &c.GitSHA, &c.Tree); err != nil {
				crow.Close()
				return nil, err
			}
			out[i].Changes = append(out[i].Changes, c)
		}
		crow.Close()
	}
	return out, nil
}

// CloseShipments sets the state of an effort's open shipments (merged when
// the effort was merged, closed when it was abandoned).
func (q Q) CloseShipments(ctx context.Context, effortID uuid.UUID, state string) error {
	_, err := q.db.Exec(ctx, `update shipments set state = $1, updated_at = now()
		where effort_id = $2 and state in ('open', 'draft', 'pushed')`, state, effortID)
	return err
}
