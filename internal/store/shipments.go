package store

import (
	"context"
	"time"

	"github.com/google/uuid"

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
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Changes     []ShipmentChange
}

// ShipmentChange is one changeset in a shipment and the commit made for it.
type ShipmentChange struct {
	Position  int
	Changeset hash.Hash
	ChangeID  string
	GitSHA    string
}

const shipmentCols = `id, effort_id, title, branch, target, base_git_sha, head_git_sha, provider_ref, url, state, created_by, created_at, updated_at`

func scanShipment(row interface{ Scan(...any) error }) (Shipment, error) {
	var s Shipment
	err := row.Scan(&s.ID, &s.EffortID, &s.Title, &s.Branch, &s.Target, &s.BaseGitSHA, &s.HeadGitSHA,
		&s.ProviderRef, &s.URL, &s.State, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// SaveShipment inserts a shipment or updates the one for the same effort
// and branch, replacing its change list.
func (q Q) SaveShipment(ctx context.Context, s Shipment) (Shipment, error) {
	row := q.db.QueryRow(ctx, `insert into shipments
		(effort_id, title, branch, target, base_git_sha, head_git_sha, provider_ref, url, state, created_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		on conflict (effort_id, branch) do update set
			title = excluded.title, target = excluded.target, base_git_sha = excluded.base_git_sha,
			head_git_sha = excluded.head_git_sha, provider_ref = excluded.provider_ref, url = excluded.url,
			state = excluded.state, updated_at = now()
		returning `+shipmentCols,
		s.EffortID, s.Title, s.Branch, s.Target, s.BaseGitSHA, s.HeadGitSHA, s.ProviderRef, s.URL, s.State, s.CreatedBy)
	out, err := scanShipment(row)
	if err != nil {
		return Shipment{}, err
	}
	if _, err := q.db.Exec(ctx, `delete from shipment_changes where shipment_id = $1`, out.ID); err != nil {
		return Shipment{}, err
	}
	for _, c := range s.Changes {
		if _, err := q.db.Exec(ctx, `insert into shipment_changes (shipment_id, position, changeset_hash, change_id, git_sha)
			values ($1, $2, $3, $4, $5)`, out.ID, c.Position, c.Changeset, c.ChangeID, c.GitSHA); err != nil {
			return Shipment{}, err
		}
	}
	out.Changes = s.Changes
	return out, nil
}

// ListShipments returns an effort's shipments with their changes.
func (q Q) ListShipments(ctx context.Context, effortID uuid.UUID) ([]Shipment, error) {
	rows, err := q.db.Query(ctx, `select `+shipmentCols+` from shipments where effort_id = $1 order by created_at`, effortID)
	if err != nil {
		return nil, err
	}
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
		crow, err := q.db.Query(ctx, `select position, changeset_hash, change_id, git_sha from shipment_changes
			where shipment_id = $1 order by position`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for crow.Next() {
			var c ShipmentChange
			if err := crow.Scan(&c.Position, &c.Changeset, &c.ChangeID, &c.GitSHA); err != nil {
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
