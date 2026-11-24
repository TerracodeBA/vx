package service

import (
	"errors"
	"time"

	"github.com/TerracodeBA/vx/internal/metrics"
)

// ForecastQueueDepth is how many repositories have overlap forecasts
// queued and not started yet, for /metrics.
func (s *Service) ForecastQueueDepth() int {
	s.forecasts.mu.Lock()
	defer s.forecasts.mu.Unlock()
	return len(s.forecasts.queued)
}

// recordLand counts a land by result.
func recordLand(res LandResult, err error) {
	var result string
	switch {
	case err != nil:
		result = "error"
		var se *Error
		if errors.As(err, &se) {
			switch se.Code {
			case CodeTooMany, CodePermission, CodeFailedPrecondition, CodeInvalid:
				result = "refused"
			}
		}
	case res.Landed:
		result = "landed"
	case len(res.Conflicts) > 0:
		result = "conflicts"
		metrics.LandConflictFiles.Add(float64(len(res.Conflicts)))
	case res.Review != nil:
		result = "review"
	case res.EffortMoved:
		result = "effort_moved"
	default:
		result = "not_landed"
	}
	metrics.Lands.Inc(result)
}

// recordForecast counts one background forecast run.
func recordForecast(d time.Duration, failed bool) {
	result := "ok"
	if failed {
		result = "error"
	}
	metrics.Forecasts.Inc(result)
	metrics.ForecastDuration.Observe(d.Seconds())
}
