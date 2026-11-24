package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
)

// FileChange is a changed path recorded with a changeset, with line counts.
type FileChange struct {
	object.Change
	Added   int
	Deleted int
}

// ChangesetRow is a changeset with the metadata vx keeps beside it.
type ChangesetRow struct {
	Hash     hash.Hash
	RepoID   uuid.UUID
	EffortID *uuid.UUID
	*object.Changeset
	// Warnings the server found in the tree when the change landed. They
	// are not part of the changeset's encoding or hash.
	Warnings []Warning
}

// Warning is a problem found in a landed tree that did not stop the land,
// such as a Go function declared twice in one package.
type Warning struct {
	Path   string   `json:"path"`
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	Decl   string   `json:"decl,omitempty"`
	Files  []string `json:"files,omitempty"`
	Detail string   `json:"detail"`
}

// SetChangesetWarnings records the warnings found when a changeset landed.
// No warnings clears the column.
func (q Q) SetChangesetWarnings(ctx context.Context, repoID uuid.UUID, h hash.Hash, ws []Warning) error {
	var raw []byte
	if len(ws) > 0 {
		var err error
		if raw, err = json.Marshal(ws); err != nil {
			return err
		}
	}
	tag, err := q.db.Exec(ctx, `update changesets set warnings = $3 where repo_id = $1 and hash = $2`, repoID, h, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("changeset %s: %w", h.Short(), ErrNotFound)
	}
	return nil
}

// InsertChangeset stores a changeset and its file list. Inserting the same
// changeset twice is a no-op, which makes imports safe to repeat.
func (q Q) InsertChangeset(ctx context.Context, repoID uuid.UUID, effortID *uuid.UUID, cs *object.Changeset, files []FileChange) (hash.Hash, error) {
	cs.Normalize()
	raw, err := cs.Encode()
	if err != nil {
		return hash.Zero, err
	}
	h := hash.Of(hash.KindChangeset, raw)
	var gitSHA *string
	if cs.GitSHA != "" {
		gitSHA = &cs.GitSHA
	}
	tag, err := q.db.Exec(ctx, `insert into changesets
		(repo_id, hash, change_id, origin, git_sha, effort_id, first_parent, tree, author_principal, title, raw, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (repo_id, hash) do nothing`,
		repoID, h, cs.ChangeID, cs.Origin, gitSHA, effortID, cs.FirstParent(), cs.Tree,
		cs.Author.Principal, cs.Title(), raw, cs.CreatedAt)
	if err != nil {
		return hash.Zero, err
	}
	if tag.RowsAffected() == 0 {
		return h, nil
	}
	for _, f := range files {
		_, err := q.db.Exec(ctx, `insert into file_changes
			(repo_id, changeset_hash, path, op, old_hash, new_hash, mode, added, deleted)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			repoID, h, f.Path, string(f.Op), f.Old.Hash, f.New.Hash, modeOf(f.Change).String(), f.Added, f.Deleted)
		if err != nil {
			return hash.Zero, fmt.Errorf("file change %s: %w", f.Path, err)
		}
	}
	return h, nil
}

func modeOf(c object.Change) object.Mode {
	if c.Op == object.OpDelete {
		return c.Old.Mode
	}
	return c.New.Mode
}

// GetChangeset loads a changeset by hash.
func (q Q) GetChangeset(ctx context.Context, repoID uuid.UUID, h hash.Hash) (ChangesetRow, error) {
	var raw, warnings []byte
	row := ChangesetRow{Hash: h, RepoID: repoID}
	err := q.db.QueryRow(ctx, `select raw, effort_id, warnings from changesets where repo_id = $1 and hash = $2`, repoID, h).
		Scan(&raw, &row.EffortID, &warnings)
	if err != nil {
		return row, notFound(err, "changeset "+h.Short())
	}
	if warnings != nil {
		if err := json.Unmarshal(warnings, &row.Warnings); err != nil {
			return row, fmt.Errorf("changeset %s has unreadable warnings: %w", h.Short(), err)
		}
	}
	cs, err := object.DecodeChangeset(raw)
	if err != nil {
		return row, fmt.Errorf("changeset %s is corrupt: %w", h.Short(), err)
	}
	if hash.Of(hash.KindChangeset, raw) != h {
		return row, fmt.Errorf("changeset %s does not match its hash", h.Short())
	}
	row.Changeset = cs
	return row, nil
}

// ChangesetByGitSHA finds the changeset imported from a git commit.
func (q Q) ChangesetByGitSHA(ctx context.Context, repoID uuid.UUID, sha string) (ChangesetRow, error) {
	var h hash.Hash
	err := q.db.QueryRow(ctx, `select hash from changesets where repo_id = $1 and git_sha = $2 limit 1`, repoID, sha).Scan(&h)
	if err != nil {
		return ChangesetRow{}, notFound(err, "commit "+sha)
	}
	return q.GetChangeset(ctx, repoID, h)
}

// ResolveChangeset finds a changeset by full hash, hex prefix (at least 6
// characters) or change ID.
func (q Q) ResolveChangeset(ctx context.Context, repoID uuid.UUID, ref string) (ChangesetRow, error) {
	if h, err := hash.Parse(ref); err == nil && !h.IsZero() {
		return q.GetChangeset(ctx, repoID, h)
	}
	if object.ValidChangeID(ref) {
		var h hash.Hash
		// The newest changeset with this change ID is the current version.
		err := q.db.QueryRow(ctx, `select hash from changesets where repo_id = $1 and change_id = $2
			order by created_at desc limit 1`, repoID, ref).Scan(&h)
		if err != nil {
			return ChangesetRow{}, notFound(err, "change "+ref)
		}
		return q.GetChangeset(ctx, repoID, h)
	}
	if isChangeIDPrefix(ref) {
		// "8tm9" or "8tm9-f": the newest changeset whose change ID starts so.
		rows, err := q.db.Query(ctx, `select distinct change_id from changesets where repo_id = $1 and change_id like $2 limit 2`, repoID, ref+"%")
		if err != nil {
			return ChangesetRow{}, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return ChangesetRow{}, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 1 {
			return q.ResolveChangeset(ctx, repoID, ids[0])
		}
		if len(ids) > 1 {
			return ChangesetRow{}, fmt.Errorf("change id prefix %q is ambiguous", ref)
		}
	}
	if len(ref) < 6 || len(ref) > 64 {
		return ChangesetRow{}, fmt.Errorf("changeset %q: %w", ref, ErrNotFound)
	}
	rows, err := q.db.Query(ctx, `select hash from changesets where repo_id = $1 and encode(hash, 'hex') like $2 limit 2`, repoID, ref+"%")
	if err != nil {
		return ChangesetRow{}, err
	}
	var found []hash.Hash
	for rows.Next() {
		var h hash.Hash
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return ChangesetRow{}, err
		}
		found = append(found, h)
	}
	rows.Close()
	switch len(found) {
	case 0:
		return ChangesetRow{}, fmt.Errorf("changeset %q: %w", ref, ErrNotFound)
	case 1:
		return q.GetChangeset(ctx, repoID, found[0])
	}
	return ChangesetRow{}, fmt.Errorf("changeset prefix %q is ambiguous", ref)
}

// Chain returns changesets from head back along first parents, stopping
// before stop (exclusive) or after limit rows. Newest first.
func (q Q) Chain(ctx context.Context, repoID uuid.UUID, head, stop hash.Hash, limit int) ([]ChangesetRow, error) {
	rows, err := q.db.Query(ctx, `with recursive chain(hash, first_parent, depth) as (
			select hash, first_parent, 1 from changesets where repo_id = $1 and hash = $2 and hash <> coalesce($3, ''::bytea)
			union all
			select c.hash, c.first_parent, chain.depth + 1
			from changesets c join chain on c.repo_id = $1 and c.hash = chain.first_parent
			where c.hash <> coalesce($3, ''::bytea) and chain.depth < $4
		)
		select hash from chain order by depth`, repoID, head, stop, limit)
	if err != nil {
		return nil, err
	}
	var hashes []hash.Hash
	for rows.Next() {
		var h hash.Hash
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ChangesetRow, 0, len(hashes))
	for _, h := range hashes {
		r, err := q.GetChangeset(ctx, repoID, h)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// FileChanges returns the recorded file list of a changeset.
func (q Q) FileChanges(ctx context.Context, repoID uuid.UUID, h hash.Hash) ([]FileChange, error) {
	rows, err := q.db.Query(ctx, `select path, op, old_hash, new_hash, mode, added, deleted
		from file_changes where repo_id = $1 and changeset_hash = $2 order by path`, repoID, h)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileChange
	for rows.Next() {
		var f FileChange
		var op, mode string
		if err := rows.Scan(&f.Path, &op, &f.Old.Hash, &f.New.Hash, &mode, &f.Added, &f.Deleted); err != nil {
			return nil, err
		}
		f.Op = object.Op(op)
		m, err := object.ParseMode(mode)
		if err != nil {
			return nil, err
		}
		if f.Op != object.OpDelete {
			f.New.Mode = m
		}
		if f.Op != object.OpAdd {
			f.Old.Mode = m
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// OnChain reports whether target is head or one of its first-parent
// ancestors, looking back at most limit steps.
func (q Q) OnChain(ctx context.Context, repoID uuid.UUID, head, target hash.Hash, limit int) (bool, error) {
	var found bool
	err := q.db.QueryRow(ctx, `with recursive chain(hash, first_parent, depth) as (
			select hash, first_parent, 1 from changesets where repo_id = $1 and hash = $2
			union all
			select c.hash, c.first_parent, chain.depth + 1
			from changesets c join chain on c.repo_id = $1 and c.hash = chain.first_parent
			where chain.hash <> $3 and chain.depth < $4
		)
		select exists (select 1 from chain where hash = $3)`, repoID, head, target, limit).Scan(&found)
	return found, err
}

// isChangeIDPrefix reports whether ref could be the start of a change ID
// ("8tm9", "8tm9-f"): at least four characters from the change ID alphabet.
func isChangeIDPrefix(ref string) bool {
	if len(ref) < 4 || len(ref) >= 9 {
		return false
	}
	for i, r := range ref {
		if i == 4 && r == '-' {
			continue
		}
		if !strings.ContainsRune("bcdfghjkmnpqrstvwxz23456789", r) {
			return false
		}
	}
	return true
}
