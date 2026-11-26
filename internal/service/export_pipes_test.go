package service

import (
	"context"

	"github.com/TerracodeBA/vx/internal/store"
)

// PublishCheckRunsForTest publishes an effort's GitHub check runs now.
func (s *Service) PublishCheckRunsForTest(ctx context.Context, repo store.Repo, eff store.Effort) {
	s.publishCheckRuns(ctx, repo, eff)
}
