package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/github/githubtest"
	"github.com/TerracodeBA/vx/internal/service"
)

func TestGitHubWebhookSignature(t *testing.T) {
	e := newEnv(t)
	ctx, svc := e.ctx, e.svc
	gh := githubtest.New(t)
	body := []byte(`{"zen":"Design for failure."}`)
	if err := svc.HandleGitHubWebhook(ctx, "ping", githubtest.NewDeliveryID(), "sha256=00", body); !errors.Is(err, service.ErrGitHubNotConfigured) {
		t.Fatalf("no App: %v", err)
	}
	creds := gh.Configure(t, svc)
	sig := githubtest.Sign(creds.WebhookSecret, body)

	id := githubtest.NewDeliveryID()
	if err := svc.HandleGitHubWebhook(ctx, "ping", id, sig, body); err != nil {
		t.Fatalf("good: %v", err)
	}
	// Replayed: the same delivery id is applied once.
	if err := svc.HandleGitHubWebhook(ctx, "ping", id, sig, body); !errors.Is(err, service.ErrGitHubDeliveryApplied) {
		t.Fatalf("replayed: %v", err)
	}
	for name, s := range map[string]string{
		"missing":      "",
		"bad":          githubtest.Sign("not the secret", body),
		"sha1":         "sha1=" + sig[len("sha256="):],
		"not hex":      "sha256=zz",
		"other body":   githubtest.Sign(creds.WebhookSecret, []byte(`{}`)),
		"empty sha256": "sha256=",
	} {
		if err := svc.HandleGitHubWebhook(ctx, "ping", githubtest.NewDeliveryID(), s, body); code(err) != service.CodePermission {
			t.Errorf("%s signature: %v", name, err)
		}
	}
	// Stale: older than three days, even with a good signature.
	old := githubtest.DeliveryIDAt(time.Now().Add(-73 * time.Hour))
	if err := svc.HandleGitHubWebhook(ctx, "ping", old, sig, body); code(err) != service.CodeInvalid {
		t.Fatalf("stale: %v", err)
	}
	recent := githubtest.DeliveryIDAt(time.Now().Add(-71 * time.Hour))
	if err := svc.HandleGitHubWebhook(ctx, "ping", recent, sig, body); err != nil {
		t.Fatalf("redelivery within three days: %v", err)
	}
	if err := svc.HandleGitHubWebhook(ctx, "ping", "", sig, body); code(err) != service.CodeInvalid {
		t.Fatalf("no delivery id: %v", err)
	}
}
