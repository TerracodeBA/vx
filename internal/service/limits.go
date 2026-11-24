package service

import (
	"context"

	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/ratelimit"
)

// AllowSignup takes one new account from the sign-up rate of the
// caller's address (VX_RATE_SIGNUP). Every path that creates an account
// from a sign-in (GitHub, single sign-on) calls it right before creating
// the person; accounts site admins create are not limited. Without
// limits, or without a known address, it allows everything.
func (s *Service) AllowSignup(ctx context.Context) error {
	return s.allowRate(ctx, ratelimit.SignUp, clientIP(ctx), "too many new accounts from your address")
}

// allowRate refuses with "slow down: WHAT; retry in N s" when key's
// bucket of class is empty.
func (s *Service) allowRate(_ context.Context, class ratelimit.Class, key, what string) error {
	if s.Limits == nil || key == "" {
		return nil
	}
	ok, wait := s.Limits.Allow(class, key)
	if ok {
		return nil
	}
	metrics.RateLimited.Inc(string(class))
	return &Error{Code: CodeTooMany, Msg: "slow down: " + what + "; " + ratelimit.RetryText(wait)}
}
