package web

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/TerracodeBA/vx/internal/service"
)

// Browser login of the CLI: `vx login --server URL` opens
// /ui/cli/approve?code=USER-CODE. People who are not signed in go through
// sign-in (u.auth sends them to /ui/login?next=...) and come back here.
// Approving is a POST from this page, so a link alone cannot authorize a
// CLI, and the page shows the code big for comparing with the terminal.

//go:embed install.sh
var installScript string

// mountCLI registers the CLI approval page and /install.sh.
func (u *UI) mountCLI(mux *http.ServeMux, funcs template.FuncMap) error {
	t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/cli_approve.html")
	if err != nil {
		return fmt.Errorf("template cli_approve: %w", err)
	}
	u.pages["cli_approve"] = t
	mux.HandleFunc("GET "+service.CLIApprovePath, u.auth(u.cliApprovePage))
	mux.HandleFunc("POST "+service.CLIApprovePath, u.auth(u.cliApprovePost))
	mux.HandleFunc("GET /install.sh", u.installScript)
	return nil
}

func (u *UI) cliApproveData(actor service.Actor, code string) map[string]any {
	return map[string]any{"Title": "Authorize vx", "Who": actor.Ref(), "Code": code}
}

func (u *UI) cliApprovePage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	// Keep the code out of other sites' logs; same-origin so the form post
	// still carries its Origin header.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	raw := strings.TrimSpace(r.URL.Query().Get("code"))
	data := u.cliApproveData(actor, service.NormalizeUserCode(raw))
	if raw == "" {
		data["Ask"] = true // no code in the link: let them type it
		u.render(w, r, "cli_approve", data)
		return
	}
	client, expires, err := u.svc.PendingCLILogin(r.Context(), actor, raw)
	if err != nil {
		data["Error"] = err.Error()
		data["Code"] = raw
		u.render(w, r, "cli_approve", data)
		return
	}
	data["Client"] = client
	data["Expires"] = expires
	data["TokenName"] = client + " (browser login)"
	u.render(w, r, "cli_approve", data)
}

func (u *UI) cliApprovePost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if !u.checkOrigin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		u.fail(w, &service.Error{Code: service.CodeInvalid, Msg: "could not read the form: " + err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	code := r.PostForm.Get("code")
	data := u.cliApproveData(actor, service.NormalizeUserCode(code))
	decision := r.PostForm.Get("decision")
	if decision != "approve" && decision != "deny" {
		data["Error"] = "choose Approve or Deny"
		u.render(w, r, "cli_approve", data)
		return
	}
	client, _, err := u.svc.PendingCLILogin(r.Context(), actor, code)
	if err == nil {
		err = u.svc.ApproveCLILogin(r.Context(), actor, code, decision == "approve")
	}
	if err != nil {
		data["Error"] = err.Error()
		data["Code"] = code
		u.render(w, r, "cli_approve", data)
		return
	}
	data["Client"] = client
	data["Done"] = decision
	u.render(w, r, "cli_approve", data)
}

// safeBase matches a base URL that can go into the script's single quotes.
var safeBase = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]+)?$`)

// installScript serves the POSIX sh installer. It names this server in
// its last step: vx login --server URL.
func (u *UI) installScript(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(u.svc.GitHub.PublicURL, "/")
	if base == "" {
		scheme := "http"
		if secure(r) {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	if !safeBase.MatchString(base) {
		base = ""
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(strings.Replace(installScript, "__VX_SERVER__", base, 1)))
}
