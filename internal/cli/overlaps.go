package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
)

// overlapsCmd lists the effort's forecast collisions with other efforts.
func (a *App) overlapsCmd() *cobra.Command {
	var repoFlag, effortFlag string
	var all bool
	cmd := &cobra.Command{
		Use:   "overlaps",
		Short: "What happens when this effort and other open efforts both ship: shared files, conflicts, rule violations",
		Long: `List the forecast collisions of the effort with the other open efforts of
the repository that have the same base branch. vx replays both efforts onto
the branch head the way shipping would and reports the files both change,
the files that would not merge, and what the combined code breaks
(duplicate Go declarations, vx.toml rules such as migration numbers).

The forecast runs after every land and when the base branch moves; vx
overlaps recomputes a stale one first. Clean pairs are left out unless you
pass --all.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			repo, slug, err := a.boardEffort(repoFlag, effortFlag)
			if err != nil {
				return err
			}
			resp, err := c.Efforts.ListOverlaps(ctxOf(cmd), connect.NewRequest(&vxv1.ListOverlapsRequest{Repo: repo, Effort: slug}))
			if err != nil {
				return a.overlapsErr(err)
			}
			os := resp.Msg.Overlaps
			shown := os
			if !all {
				shown = notClean(os)
			}
			switch {
			case len(os) == 0:
				a.printf("No other open efforts on the same base branch as %s.\n", slug)
			case len(shown) == 0:
				a.printf("No overlaps: %s shares no files with the %d other open effort(s), and shipping them together breaks no rule.\n", slug, len(os))
			default:
				a.printf("Overlaps of %s with other efforts (%d of %d):\n", slug, len(shown), len(os))
				if !a.jsonOut {
					writeOverlaps(a.Out, shown)
				}
				if hasConflict(shown) {
					a.printf("Agree with the other effort's owner on the board who changes what, or rebase after the other ships.\n")
				}
			}
			if !all {
				resp.Msg.Overlaps = shown
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	boardScopeFlags(cmd, &repoFlag, &effortFlag)
	cmd.Flags().BoolVar(&all, "all", false, "also list the efforts that do not collide")
	return cmd
}

// overlapsErr explains a server without the forecast.
func (a *App) overlapsErr(err error) error {
	switch client.Code(err) {
	case connect.CodeUnimplemented:
		return a.featureErr("overlaps", "overlap forecasts", err)
	case connect.CodeUnavailable:
		if strings.Contains(client.Message(err), "not implemented") {
			return a.featureErr("overlaps", "overlap forecasts", err)
		}
	}
	return err
}

func notClean(os []*vxv1.Overlap) []*vxv1.Overlap {
	var out []*vxv1.Overlap
	for _, o := range os {
		if o.Status != "clean" {
			out = append(out, o)
		}
	}
	return out
}

func hasConflict(os []*vxv1.Overlap) bool {
	for _, o := range os {
		if o.Status == "conflict" {
			return true
		}
	}
	return false
}

// printOverlaps shows forecast collisions after a land or sync, after the
// warnings and before the board feed. Clean pairs are left out.
func (a *App) printOverlaps(os []*vxv1.Overlap) {
	os = notClean(os)
	if len(os) == 0 || a.jsonOut {
		return
	}
	a.printf("Overlaps with other efforts:\n")
	writeOverlaps(a.Out, os)
}

// overlapStatus prints the Overlaps section of vx status and returns it for
// --json. Servers without the forecast print nothing.
func (a *App) overlapStatus(ctx context.Context, c *client.Client, repo, effort string) []map[string]any {
	resp, err := c.Efforts.ListOverlaps(ctx, connect.NewRequest(&vxv1.ListOverlapsRequest{Repo: repo, Effort: effort}))
	if err != nil {
		return nil
	}
	os := notClean(resp.Msg.Overlaps)
	out := []map[string]any{}
	for _, o := range os {
		out = append(out, map[string]any{"effort": o.OtherEffort, "owner": o.OtherOwner, "status": o.Status,
			"shared_files": o.SharedFiles, "conflicts": o.Conflicts, "line": overlapLine(o)})
	}
	if len(os) > 0 {
		a.printf("\nOverlaps with other efforts (details: vx overlaps):\n")
		writeOverlapsTo(a, os)
	}
	return out
}

func writeOverlapsTo(a *App, os []*vxv1.Overlap) {
	if !a.jsonOut {
		writeOverlaps(a.Out, os)
	}
}

// printOverlaps writes one line per collision to w (vx context uses it).
func printOverlaps(w io.Writer, os []*vxv1.Overlap) {
	writeOverlaps(w, notClean(os))
}

func writeOverlaps(w io.Writer, os []*vxv1.Overlap) {
	for _, o := range os {
		fmt.Fprintf(w, "  %s\n", overlapLine(o))
	}
}

// overlapLine is one collision: "conflict  audit-log (user:bob): conflicts in auth.go".
func overlapLine(o *vxv1.Overlap) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-9s %s (%s): ", o.Status, o.OtherEffort, o.OtherOwner)
	var parts []string
	if len(o.Conflicts) > 0 {
		parts = append(parts, "conflicts in "+listFew(o.Conflicts))
	}
	if shared := len(o.SharedFiles) - len(o.Conflicts); shared > 0 && len(o.Conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("%d more shared file(s) merge", shared))
	} else if len(o.Conflicts) == 0 && len(o.SharedFiles) > 0 {
		parts = append(parts, "shares "+listFew(o.SharedFiles)+", merges cleanly")
	}
	for _, w := range o.Warnings {
		parts = append(parts, strings.TrimSuffix(w.Detail, "."))
	}
	if len(parts) == 0 {
		parts = append(parts, "no shared files")
	}
	b.WriteString(strings.Join(parts, "; "))
	return b.String()
}

func listFew(paths []string) string {
	const max = 5
	if len(paths) <= max {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:max], ", "), len(paths)-max)
}
