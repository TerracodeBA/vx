package service

// Who is working on what: open workspaces report the files their working
// copy changed, so vx context can name those working on the files asked
// about before everyone else in the effort.

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// MaxReportedPaths is the most changed files a workspace reports; more are
// cut (sorted, the first ones kept).
const MaxReportedPaths = 2000

// ReportChanges records the files the caller's open workspace has changed
// locally (write). vx status, vx sync, vx claim and vx wait send them.
func (s *Service) ReportChanges(ctx context.Context, actor Actor, workspaceID uuid.UUID, paths []string) error {
	v, err := s.ownWorkspace(ctx, actor, workspaceID)
	if err != nil {
		return err
	}
	if v.Workspace.State != store.WorkspaceOpen {
		return errf(CodeFailedPrecondition, "workspace %s is already %s; open a new one with vx new \"INTENT\"", v.Workspace.ChangeID, v.Workspace.State)
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(stripControls(p))
		if p == "" || seen[p] || utf8.RuneCountInString(p) > 4096 {
			continue
		}
		seen[p] = true
		clean = append(clean, p)
	}
	sort.Strings(clean)
	if len(clean) > MaxReportedPaths {
		clean = clean[:MaxReportedPaths]
	}
	return s.Store.SetWorkspaceChangedPaths(ctx, workspaceID, clean, s.Now())
}

// contextWorking lists open workspaces for Context: those on the targets
// (changed files or claims matching them) first, then the others, each
// group oldest first.
func (s *Service) contextWorking(ctx context.Context, c *noteCache, workspaces []store.Workspace, targets []string, claims []store.Claim) ([]ContextWorkspace, error) {
	var on, rest []ContextWorkspace
	for _, w := range workspaces {
		owner, err := c.principal(ctx, w.OwnerID)
		if err != nil {
			return nil, err
		}
		cw := ContextWorkspace{Workspace: w, Owner: owner, FilesKnown: w.ChangedPathsAt != nil}
		if len(targets) > 0 {
			seen := map[string]bool{}
			add := func(p string) {
				if !seen[p] {
					seen[p] = true
					cw.OnTargets = append(cw.OnTargets, p)
				}
			}
			for _, p := range w.ChangedPaths {
				if matchAny([]string{p}, targets) {
					add(p)
				}
			}
			for _, cl := range claims { // claims passed in match the targets already
				if cl.WorkspaceID != nil && *cl.WorkspaceID == w.ID {
					for _, t := range cl.Targets {
						if matchAny([]string{t}, targets) {
							add(t)
						}
					}
				}
			}
		}
		if len(cw.OnTargets) > 0 {
			on = append(on, cw)
		} else {
			rest = append(rest, cw)
		}
	}
	return append(on, rest...), nil
}
