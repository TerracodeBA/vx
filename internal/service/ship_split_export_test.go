package service

import (
	"context"

	"github.com/TerracodeBA/vx/internal/store"
)

// MarkShipmentMergedByHead is markShipmentMergedByHead (tests only): what a
// merged pull request webhook does for a squash merge.
func (s *Service) MarkShipmentMergedByHead(ctx context.Context, repo store.Repo, head string) (bool, error) {
	return s.markShipmentMergedByHead(ctx, repo, head, "its pull request was merged")
}
