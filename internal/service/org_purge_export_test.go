package service

// SetPurgeHook runs f before every batch of a purge (tests only).
func SetPurgeHook(s *Service, f func(step string)) { s.purgeHook = f }
