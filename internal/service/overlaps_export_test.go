package service

// SetForecastHook runs f before each background forecast (tests only).
func (s *Service) SetForecastHook(f func()) {
	s.forecasts.mu.Lock()
	defer s.forecasts.mu.Unlock()
	s.forecasts.hook = f
}
