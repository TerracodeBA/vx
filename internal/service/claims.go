package service

// Claims: soft "I am changing these paths or symbols now" markers. The
// contract (types and signatures) is in coordination.go; this file
// implements it. Claims never block: MakeClaim succeeds whatever others
// hold, and Land only reports the claims it landed over.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// MakeClaim records a claim and returns it with the active claims of others
// on the same targets. It never fails because of them.
func (s *Service) MakeClaim(ctx context.Context, actor Actor, in ClaimInput) (Claim, []Claim, error) {
	targets, err := cleanTargets(in.Targets)
	if err != nil {
		return Claim{}, nil, err
	}
	if len(targets) == 0 {
		return Claim{}, nil, errf(CodeInvalid, "say what you are claiming: give a path (internal/http/limit.go, internal/http/) or a symbol (auth.Login)")
	}
	reason, err := cleanReason(in.Reason)
	if err != nil {
		return Claim{}, nil, err
	}
	ttl := in.TTL
	switch {
	case ttl == 0:
		ttl = DefaultClaimTTL
	case ttl < 0:
		return Claim{}, nil, errf(CodeInvalid, "claim duration %s is negative; give a positive duration such as 30m", ttl)
	case ttl > MaxClaimTTL:
		return Claim{}, nil, errf(CodeInvalid, "claim duration %s is too long; claim for at most %s and claim again when it runs out", ttl, MaxClaimTTL)
	}
	v, err := s.boardEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return Claim{}, nil, err
	}
	if v.Effort.State != store.EffortOpen {
		return Claim{}, nil, errf(CodeFailedPrecondition, "effort %s is %s; claims are for open efforts", v.Effort.Slug, v.Effort.State)
	}
	var wsID *uuid.UUID
	if id := strings.TrimSpace(in.WorkspaceID); id != "" {
		ws, err := s.claimWorkspace(ctx, actor, v.Effort, id)
		if err != nil {
			return Claim{}, nil, err
		}
		wsID = &ws.ID
	}
	now := s.Now()
	var row store.Claim
	var others []store.Claim
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		row, err = tx.InsertClaim(ctx, store.Claim{
			EffortID: v.Effort.ID, OwnerID: actor.ID, WorkspaceID: wsID, Targets: targets, Reason: reason,
			CreatedAt: now, ExpiresAt: now.Add(ttl),
		})
		if err != nil {
			return err
		}
		after := map[string]any{"effort": row.EffortSlug, "targets": row.Targets, "expires_at": row.ExpiresAt}
		if wsID != nil {
			after["workspace"] = wsID.String()
		}
		if err := tx.LogOp(ctx, &row.RepoID, &actor.ID, "claim.make", row.ID.String(), nil, after); err != nil {
			return err
		}
		others, err = s.activeClaims(ctx, tx.Q, v.Effort.ID, targets, actor.ID)
		return err
	})
	if err != nil {
		return Claim{}, nil, classify(err)
	}
	cc := s.newNoteCache()
	claim, err := cc.claim(ctx, row)
	if err != nil {
		return Claim{}, nil, err
	}
	overlapping, err := cc.claims(ctx, others)
	return claim, overlapping, err
}

// ReleaseClaims ends one claim (id or prefix) or all of the caller's claims
// for a workspace, and returns what it released.
func (s *Service) ReleaseClaims(ctx context.Context, actor Actor, id, workspaceID string) ([]Claim, error) {
	id, workspaceID = strings.TrimSpace(id), strings.TrimSpace(workspaceID)
	switch {
	case id == "" && workspaceID == "":
		return nil, errf(CodeInvalid, "say which claim to release: its id (`vx claims` lists them), or a workspace to release all of your claims for it")
	case id != "" && workspaceID != "":
		return nil, errf(CodeInvalid, "give either a claim id or a workspace, not both")
	}
	now := s.Now()
	var rows []store.Claim
	if id != "" {
		found, err := s.findClaim(ctx, id)
		if err != nil {
			return nil, err
		}
		if found.OwnerID != actor.ID {
			return nil, errf(CodePermission, "claim %s belongs to someone else; only its owner releases it, and it ends on its own at %s",
				shortID(found.ID), found.ExpiresAt.UTC().Format(time.RFC3339))
		}
		err = s.Store.InTx(ctx, func(tx *store.Tx) error {
			c, ok, err := tx.ReleaseClaim(ctx, found.ID, now)
			if err != nil {
				return err
			}
			if !ok {
				if c.ReleasedAt != nil {
					return errf(CodeFailedPrecondition, "claim %s was already released", shortID(c.ID))
				}
				return errf(CodeFailedPrecondition, "claim %s already expired at %s", shortID(c.ID), c.ExpiresAt.UTC().Format(time.RFC3339))
			}
			rows = []store.Claim{c}
			return tx.LogOp(ctx, &c.RepoID, &actor.ID, "claim.release", c.ID.String(), nil, map[string]any{"effort": c.EffortSlug, "targets": c.Targets})
		})
		if err != nil {
			return nil, classify(err)
		}
	} else {
		wsID, err := uuid.Parse(workspaceID)
		if err != nil {
			return nil, errf(CodeInvalid, "workspace id %q is not a full id; `vx status` shows the open workspace", workspaceID)
		}
		ws, err := s.GetWorkspace(ctx, wsID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errf(CodeNotFound, "no workspace %s", workspaceID)
			}
			return nil, err
		}
		err = s.Store.InTx(ctx, func(tx *store.Tx) error {
			var err error
			rows, err = s.releaseWorkspaceClaims(ctx, tx, actor, ws.Repo.ID, wsID, &actor.ID, now)
			return err
		})
		if err != nil {
			return nil, classify(err)
		}
	}
	return s.newNoteCache().claims(ctx, rows)
}

// ListClaims returns the active claims of an effort, optionally only those
// matching targets (TargetsMatch).
func (s *Service) ListClaims(ctx context.Context, actor Actor, repo, effort string, targets []string) ([]Claim, error) {
	targets, err := cleanTargets(targets)
	if err != nil {
		return nil, err
	}
	v, err := s.boardEffort(ctx, repo, effort)
	if err != nil {
		return nil, err
	}
	rows, err := s.activeClaims(ctx, s.Store.Q, v.Effort.ID, targets, uuid.Nil)
	if err != nil {
		return nil, err
	}
	return s.newNoteCache().claims(ctx, rows)
}

// activeClaims lists an effort's active claims whose targets match want
// (all when want is empty), leaving out those of skip.
func (s *Service) activeClaims(ctx context.Context, q store.Q, effortID uuid.UUID, want []string, skip uuid.UUID) ([]store.Claim, error) {
	rows, err := q.ActiveClaims(ctx, effortID, s.Now())
	if err != nil {
		return nil, err
	}
	out := []store.Claim{}
	for _, c := range rows {
		if c.OwnerID != skip && (len(want) == 0 || matchAny(c.Targets, want)) {
			out = append(out, c)
		}
	}
	return out, nil
}

// landedOverClaims returns the active claims of principals other than the
// caller whose targets match a file the land changed.
func (s *Service) landedOverClaims(ctx context.Context, tx *store.Tx, actor Actor, effortID uuid.UUID, files []store.FileChange) ([]Claim, error) {
	if len(files) == 0 {
		return nil, nil
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	rows, err := s.activeClaims(ctx, tx.Q, effortID, paths, actor.ID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return s.newNoteCache().claims(ctx, rows)
}

// releaseWorkspaceClaims releases a workspace's active claims inside tx and
// logs each release. A non-nil owner limits it to that principal's claims.
func (s *Service) releaseWorkspaceClaims(ctx context.Context, tx *store.Tx, actor Actor, repoID, wsID uuid.UUID, owner *uuid.UUID, now time.Time) ([]store.Claim, error) {
	rows, err := tx.ReleaseWorkspaceClaims(ctx, wsID, owner, now)
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		if err := tx.LogOp(ctx, &repoID, &actor.ID, "claim.release", c.ID.String(), nil,
			map[string]any{"effort": c.EffortSlug, "targets": c.Targets, "workspace": wsID.String()}); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// claimWorkspace checks that id is an open workspace of the caller in eff.
func (s *Service) claimWorkspace(ctx context.Context, actor Actor, eff store.Effort, id string) (store.Workspace, error) {
	wsID, err := uuid.Parse(id)
	if err != nil {
		return store.Workspace{}, errf(CodeInvalid, "workspace id %q is not a full id; `vx status` shows the open workspace", id)
	}
	ws, err := s.Store.GetWorkspace(ctx, wsID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Workspace{}, errf(CodeNotFound, "no workspace %s; open one with vx new \"INTENT\" or claim without a workspace", id)
	case err != nil:
		return store.Workspace{}, err
	case ws.OwnerID != actor.ID:
		return store.Workspace{}, errf(CodePermission, "workspace %s belongs to another principal; tie claims to your own workspace", id)
	case ws.EffortID != eff.ID:
		return store.Workspace{}, errf(CodeInvalid, "workspace %s is in another effort, not %s", id, eff.Slug)
	case ws.State != store.WorkspaceOpen:
		return store.Workspace{}, errf(CodeFailedPrecondition, "workspace %s is already %s; open a new one with vx new \"INTENT\"", id, ws.State)
	}
	return ws, nil
}

// findClaim resolves a full claim id or a unique prefix of at least
// MinNoteIDPrefix characters.
func (s *Service) findClaim(ctx context.Context, id string) (store.Claim, error) {
	id = strings.ToLower(id)
	if full, err := uuid.Parse(id); err == nil {
		c, err := s.Store.ClaimByID(ctx, full)
		if errors.Is(err, store.ErrNotFound) {
			return store.Claim{}, errf(CodeNotFound, "no claim %s; `vx claims` lists the effort's active claims", id)
		}
		return c, err
	}
	if len(id) < MinNoteIDPrefix {
		return store.Claim{}, errf(CodeInvalid, "claim id %q is too short: give at least %d characters", id, MinNoteIDPrefix)
	}
	c, err := s.Store.ClaimByPrefix(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Claim{}, errf(CodeNotFound, "no claim has an id starting with %s; `vx claims` lists the effort's active claims", id)
	case errors.Is(err, store.ErrAmbiguous):
		return store.Claim{}, errf(CodeInvalid, "several claims have ids starting with %s; give more characters", id)
	}
	return c, err
}

// cleanReason turns line breaks and tabs into spaces, drops other control
// characters and bidirectional overrides, and checks the length. A reason
// is optional.
func cleanReason(reason string) (string, error) {
	var b strings.Builder
	for _, r := range reason {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r) || isBidiControl(r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if n := utf8.RuneCountInString(out); n > MaxClaimReason {
		return "", errf(CodeInvalid, "the reason is %d characters; keep it to %d and post a plan note for details", n, MaxClaimReason)
	}
	return out, nil
}

func (c *noteCache) claim(ctx context.Context, r store.Claim) (Claim, error) {
	out := Claim{
		ID: r.ID, RepoName: r.RepoName, EffortSlug: r.EffortSlug, EffortID: r.EffortID, Targets: r.Targets,
		WorkspaceID: r.WorkspaceID, Reason: r.Reason, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ReleasedAt: r.ReleasedAt,
	}
	var err error
	if out.Owner, err = c.principal(ctx, r.OwnerID); err != nil {
		return Claim{}, err
	}
	if out.Owner.ParentID != nil {
		parent, err := c.principal(ctx, *out.Owner.ParentID)
		if err != nil {
			return Claim{}, err
		}
		out.OwnerParent = &parent
	}
	return out, nil
}

func (c *noteCache) claims(ctx context.Context, rows []store.Claim) ([]Claim, error) {
	out := make([]Claim, 0, len(rows))
	for _, r := range rows {
		cl, err := c.claim(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, nil
}
