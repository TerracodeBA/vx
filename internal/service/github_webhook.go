package service

// Webhook deliveries from GitHub: verified with the App's webhook secret,
// refused when stale, applied once per delivery id.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// githubDeliveryMaxAge is how old a delivery may be: GitHub redelivers
	// deliveries of the last three days. Older ones are refused.
	githubDeliveryMaxAge = 72 * time.Hour
	// githubDeliveryKeep is how long delivery ids are kept to refuse
	// replays; longer than the age limit, so every replay is caught.
	githubDeliveryKeep = 7 * 24 * time.Hour
)

// ErrGitHubDeliveryApplied is returned for a delivery applied before.
var ErrGitHubDeliveryApplied = &Error{Code: CodeExists, Msg: "this delivery was applied already"}

// verifyGitHubSignature checks X-Hub-Signature-256 in constant time.
func verifyGitHubSignature(secret, signature string, body []byte) bool {
	hexSig, ok := strings.CutPrefix(signature, "sha256=")
	if !ok || secret == "" {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// deliveryTime is when GitHub made a delivery: its id is a version 1 UUID,
// which carries the time. ok is false for other ids.
func deliveryTime(id string) (time.Time, bool) {
	u, err := uuid.Parse(id)
	if err != nil || u.Version() != 1 {
		return time.Time{}, false
	}
	sec, nsec := u.Time().UnixTime()
	return time.Unix(sec, nsec), true
}

// HandleGitHubWebhook verifies and applies a webhook delivery:
// installations, repositories, membership and permission changes.
func (s *Service) HandleGitHubWebhook(ctx context.Context, event, deliveryID, signature string, body []byte) error {
	secret, err := s.githubWebhookSecret(ctx)
	if err != nil {
		return err
	}
	if signature == "" {
		return errf(CodePermission, "unsigned delivery: GitHub signs every delivery with the App's webhook secret (X-Hub-Signature-256)")
	}
	if !verifyGitHubSignature(secret, signature, body) {
		return errf(CodePermission, "the delivery's signature does not match the App's webhook secret")
	}
	if event == "" || deliveryID == "" || len(deliveryID) > 100 {
		return errf(CodeInvalid, "a delivery needs X-GitHub-Event and X-GitHub-Delivery")
	}
	if at, ok := deliveryTime(deliveryID); ok {
		if age := s.Clock().Sub(at); age > githubDeliveryMaxAge {
			return errf(CodeInvalid, "stale delivery from %s: vx applies deliveries of the last three days", at.UTC().Format(time.RFC3339))
		}
	}
	if !json.Valid(body) {
		return errf(CodeInvalid, "the delivery is not JSON")
	}
	fresh, err := s.Store.RecordGitHubDelivery(ctx, deliveryID, event, githubDeliveryKeep)
	if err != nil {
		return err
	}
	if !fresh {
		return ErrGitHubDeliveryApplied
	}
	if err := s.applyGitHubEvent(ctx, event, body); err != nil {
		// Let GitHub's redelivery try again.
		if ferr := s.Store.ForgetGitHubDelivery(context.WithoutCancel(ctx), deliveryID); ferr != nil {
			return errors.Join(err, ferr)
		}
		return err
	}
	return nil
}
