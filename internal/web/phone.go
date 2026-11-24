package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// ---------- phones: home screen app, offline page, push (v0.5, #37) ----------

// mountPhone registers the For you page, the web app manifest, the service
// worker, the offline page and the notification settings.
func (u *UI) mountPhone(mux *http.ServeMux, funcs template.FuncMap) error {
	for _, page := range []string{"foryou", "offline"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/"+page+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", page, err)
		}
		u.pages[page] = t
	}
	mux.HandleFunc("GET /ui/for-you", u.auth(u.forYou))
	mux.HandleFunc("POST /ui/for-you/answer", u.auth(u.forYouAnswerPost))
	mux.HandleFunc("POST /ui/for-you/review", u.auth(u.forYouReviewPost))
	mux.HandleFunc("GET /ui/manifest.webmanifest", staticFile("static/manifest.webmanifest", "application/manifest+json", "public, max-age=3600"))
	// The worker lives at /ui/ so its scope covers every page; browsers
	// check it for updates, so it is never cached long.
	mux.HandleFunc("GET /ui/sw.js", staticFile("static/sw.js", "text/javascript; charset=utf-8", "no-cache"))
	mux.HandleFunc("GET /ui/offline", u.offline)
	mux.HandleFunc("GET /ui/push/key", u.auth(u.pushKey))
	mux.HandleFunc("POST /ui/push/subscribe", u.auth(u.pushSubscribePost))
	mux.HandleFunc("POST /ui/push/unsubscribe", u.auth(u.pushUnsubscribePost))
	mux.HandleFunc("POST /ui/push/test", u.auth(u.pushTestPost))
	return nil
}

// staticFile serves one embedded file with a fixed type (Go's MIME table
// does not know .webmanifest, and a worker must be JavaScript).
func staticFile(name, contentType, cache string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cache)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(b)
	}
}

// offline is what the service worker shows when a page cannot load. It
// needs no session: the worker fetches it once when installed.
func (u *UI) offline(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "offline", map[string]any{"Title": "Offline"})
}

// pushDevice is a browser with notifications on, for Settings.
type pushDevice struct {
	store.PushSubscription
	Name string
}

// settingsPush adds the notification panel's data to the Settings page.
func (u *UI) settingsPush(r *http.Request, actor service.Actor, data map[string]any) {
	if actor.Kind != store.KindHuman {
		return
	}
	data["PushAvailable"] = u.svc.PushAvailable()
	subs, err := u.svc.PushSubscriptions(r.Context(), actor)
	if err != nil {
		data["PushErr"] = err.Error()
		return
	}
	devs := make([]pushDevice, 0, len(subs))
	for _, s := range subs {
		d := pushDevice{PushSubscription: s, Name: s.Device}
		if d.Name == "" {
			d.Name = "a browser"
		}
		devs = append(devs, d)
	}
	data["PushDevices"] = devs
	switch param(r, "push") {
	case "off":
		data["PushFlash"] = "Notifications are off in that browser."
	}
}

// wantsJSON reports whether the page's script (not a plain form) asked.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (u *UI) pushJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pushFail answers a script with JSON and a form with the error page.
func (u *UI) pushFail(w http.ResponseWriter, r *http.Request, err error) {
	if !wantsJSON(r) {
		u.fail(w, err)
		return
	}
	status := http.StatusBadRequest
	var se *service.Error
	if errors.As(service.Classify(err), &se) && se.Code == service.CodePermission {
		status = http.StatusForbidden
	}
	u.pushJSON(w, status, map[string]string{"error": err.Error()})
}

// pushKey is the server's public VAPID key, for PushManager.subscribe.
func (u *UI) pushKey(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	key, err := u.svc.PushPublicKey(r.Context())
	if err != nil {
		u.pushJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	u.pushJSON(w, http.StatusOK, map[string]string{"key": key})
}

func (u *UI) pushSubscribePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.pushFail(w, r, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	sub, err := u.svc.SubscribePush(r.Context(), actor, service.PushSubscribeInput{
		Endpoint: r.PostForm.Get("endpoint"), P256dh: r.PostForm.Get("p256dh"), Auth: r.PostForm.Get("auth"), Device: r.PostForm.Get("device"),
	})
	if err != nil {
		u.pushFail(w, r, err)
		return
	}
	if wantsJSON(r) {
		u.pushJSON(w, http.StatusOK, map[string]string{"id": sub.ID.String()})
		return
	}
	http.Redirect(w, r, "/ui/settings#notifications", http.StatusSeeOther)
}

func (u *UI) pushUnsubscribePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.pushFail(w, r, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	if err := u.svc.UnsubscribePush(r.Context(), actor, r.PostForm.Get("endpoint")); err != nil {
		u.pushFail(w, r, err)
		return
	}
	if wantsJSON(r) {
		u.pushJSON(w, http.StatusOK, map[string]string{})
		return
	}
	http.Redirect(w, r, "/ui/settings?push=off#notifications", http.StatusSeeOther)
}

func (u *UI) pushTestPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.pushFail(w, r, err)
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	if err := u.svc.TestPush(r.Context(), actor, r.PostForm.Get("endpoint")); err != nil {
		u.pushFail(w, r, err)
		return
	}
	u.pushJSON(w, http.StatusOK, map[string]string{"sent": "yes"})
}
