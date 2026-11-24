package store

import "context"

// SetBranchIfUnchanged moves a tracked branch to b only while it is still
// at git commit fromSHA, or, when it was not tracked (tracked false), only
// while it still is not. It reports whether it moved;
// false means another server moved the branch first. Imports use it so two
// servers syncing one branch at once never move it back to an older commit.
func (q Q) SetBranchIfUnchanged(ctx context.Context, b Branch, fromSHA string, tracked bool) (bool, error) {
	if !tracked {
		tag, err := q.db.Exec(ctx, `insert into tracked_branches (repo_id, name, head, git_sha, synced_at)
			values ($1, $2, $3, $4, now()) on conflict (repo_id, name) do nothing`,
			b.RepoID, b.Name, b.Head, b.GitSHA)
		return err == nil && tag.RowsAffected() == 1, err
	}
	tag, err := q.db.Exec(ctx, `update tracked_branches set head = $3, git_sha = $4, synced_at = now()
		where repo_id = $1 and name = $2 and git_sha = $5`, b.RepoID, b.Name, b.Head, b.GitSHA, fromSHA)
	return err == nil && tag.RowsAffected() == 1, err
}
