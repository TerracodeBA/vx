package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// mountHostHooks receives the webhooks of GitLab, Gitea, Forgejo and
// Bitbucket Cloud. The service checks the secret; nothing here trusts the
// body before that.
func (u *UI) mountHostHooks(mux *http.ServeMux) {
	mux.HandleFunc("POST "+service.GitLabWebhookPath, u.hostWebhook(store.ProviderGitLab))
	mux.HandleFunc("POST "+service.GiteaWebhookPath, u.hostWebhook(store.ProviderGitea))
	mux.HandleFunc("POST "+service.ForgejoWebhookPath, u.hostWebhook(store.ProviderForgejo))
	mux.HandleFunc("POST "+service.BitbucketWebhookPath, u.hostWebhook(store.ProviderBitbucket))
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func (u *UI) hostWebhook(kind store.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "payload too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		d := service.HostDelivery{Body: body}
		switch kind {
		case store.ProviderGitLab:
			d.Event = r.Header.Get("X-Gitlab-Event")
			d.ID = firstHeader(r, "X-Gitlab-Event-UUID", "X-Gitlab-Webhook-UUID")
			d.Token = r.Header.Get("X-Gitlab-Token")
		case store.ProviderGitea, store.ProviderForgejo:
			d.Event = firstHeader(r, "X-Forgejo-Event", "X-Gitea-Event")
			d.ID = firstHeader(r, "X-Forgejo-Delivery", "X-Gitea-Delivery")
			d.Signature = firstHeader(r, "X-Forgejo-Signature", "X-Gitea-Signature", "X-Hub-Signature-256")
		case store.ProviderBitbucket:
			d.Event = r.Header.Get("X-Event-Key")
			d.ID = r.Header.Get("X-Request-UUID")
			d.Signature = firstHeader(r, "X-Hub-Signature-256", "X-Hub-Signature")
			d.URLSecret = r.URL.Query().Get("secret")
		}
		err = u.svc.HandleHostWebhook(r.Context(), kind, d)
		status, msg := http.StatusOK, "ok"
		var se *service.Error
		switch {
		case err == nil:
		case errors.As(service.Classify(err), &se):
			switch se.Code {
			case service.CodeExists: // a replay of a delivery already applied
				status, msg = http.StatusOK, se.Msg
			case service.CodePermission:
				status, msg = http.StatusUnauthorized, se.Msg
			case service.CodeInvalid, service.CodeFailedPrecondition:
				status, msg = http.StatusBadRequest, se.Msg
			default:
				status, msg = http.StatusInternalServerError, se.Msg
			}
		default:
			status, msg = http.StatusInternalServerError, "could not apply the delivery"
		}
		if status >= 500 || status == http.StatusUnauthorized {
			u.log.Warn(string(kind)+" webhook", "event", d.Event, "delivery", d.ID, "status", status, "err", err)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintln(w, msg)
	}
}
