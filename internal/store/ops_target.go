package store

import "context"

// ListTargetOps returns the newest operations on one target ("user:alice",
// "github:octocat"), whoever performed them; console operations have no
// actor.
func (q Q) ListTargetOps(ctx context.Context, target string, limit int) ([]Op, error) {
	rows, err := q.db.Query(ctx, `select id, repo_id, actor_id, op, target, before, after, at
		from op_log where target = $1 order by id desc limit $2`, target, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Op
	for rows.Next() {
		var o Op
		if err := rows.Scan(&o.ID, &o.RepoID, &o.ActorID, &o.Op, &o.Target, &o.Before, &o.After, &o.At); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
