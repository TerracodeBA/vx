package cli

// Review and CI feedback from the git host in the CLI (v0.4): the CI line
// of a shipment (vx ship list, vx status) and the system notes vx posts to
// an agent (review comments, CI results, rebases), which vx inbox, the
// sync and land feed and vx wait bring.

import (
	"context"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
)

// feedbackKinds are the system notes vx addresses to one agent.
var feedbackKinds = map[string]bool{"review": true, "ci": true}

// ciLine is "CI: passed", "CI: failed (go test, URL; lint, URL)",
// "CI: pending (2 running)" or "" when the host reported nothing.
func ciLine(s *vxv1.Shipment) string {
	switch s.CiState {
	case "":
		return ""
	case "failed":
		var parts []string
		for _, c := range s.CiChecks {
			if c.Status != "completed" || c.Source == "check_suite" && hasRuns(s.CiChecks) {
				continue
			}
			switch c.Conclusion {
			case "success", "neutral", "skipped":
				continue
			}
			p := c.Name
			if c.Url != "" {
				p += ", " + c.Url
			}
			parts = append(parts, p)
		}
		if len(parts) == 0 {
			return "CI: failed"
		}
		return "CI: failed (" + strings.Join(parts, "; ") + ")"
	case "pending":
		n := 0
		for _, c := range s.CiChecks {
			if c.Status != "completed" {
				n++
			}
		}
		if n > 0 {
			return "CI: pending (" + strconv.Itoa(n) + " running)"
		}
	}
	return "CI: " + s.CiState
}

func hasRuns(cs []*vxv1.ShipmentCheck) bool {
	for _, c := range cs {
		if c.Source == "check_run" {
			return true
		}
	}
	return false
}

// shipStatus prints the effort's open shipments with their CI for vx
// status and returns them for --json; nil when there are none.
func (a *App) shipStatus(ctx context.Context, c *client.Client, repo, effort string) []map[string]string {
	resp, err := c.Ships.ListShipments(ctx, connect.NewRequest(&vxv1.ListShipmentsRequest{Repo: repo, Effort: effort}))
	if err != nil || len(resp.Msg.Shipments) == 0 {
		return nil
	}
	var out []map[string]string
	for _, s := range resp.Msg.Shipments {
		if s.State == "merged" || s.State == "closed" {
			continue
		}
		where := s.Branch
		if s.PrNumber != "" {
			where = "#" + s.PrNumber + " " + s.Url
		}
		line := ciLine(s)
		if len(out) == 0 {
			a.printf("\nShipped:\n")
		}
		if line != "" {
			a.printf("  %s %s  %s\n", s.State, where, line)
		} else {
			a.printf("  %s %s\n", s.State, where)
		}
		out = append(out, map[string]string{"branch": s.Branch, "pr": s.PrNumber, "url": s.Url, "state": s.State, "ci": s.CiState})
	}
	return out
}
