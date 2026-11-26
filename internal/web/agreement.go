package web

// The data processing agreement on the organization page (v0.5, #41):
// the version to accept, rendered from the operator's Markdown, with a
// checkbox and Accept; every past acceptance with the text exactly as
// accepted, to read and download. Only with VX_REQUIRE_DPA on.

import (
	"bytes"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// agreementMarkdown renders the operator's Markdown. Raw HTML in it is left
// out and dangerous links (javascript:) are dropped, and nothing it makes
// needs inline styles or scripts, so the strict CSP holds: table cells are
// aligned with the align attribute, not style.
var agreementMarkdown = goldmark.New(
	goldmark.WithExtensions(
		extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)),
		extension.Strikethrough,
	),
)

// renderAgreement turns an agreement's Markdown into safe HTML.
func renderAgreement(text []byte) (template.HTML, error) {
	var b bytes.Buffer
	if err := agreementMarkdown.Convert(text, &b); err != nil {
		return "", err
	}
	return template.HTML(b.String()), nil //nolint:gosec // goldmark without WithUnsafe escapes raw HTML
}

func (u *UI) mountAgreement(mux *http.ServeMux, funcs template.FuncMap) error {
	t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/agreement.html")
	if err != nil {
		return fmt.Errorf("template agreement: %w", err)
	}
	u.pages["agreement"] = t
	mux.HandleFunc("POST /ui/orgs/{org}/agreement", u.auth(u.agreementAcceptPost))
	mux.HandleFunc("GET /ui/orgs/{org}/agreement", u.auth(u.agreementPage))
	mux.HandleFunc("GET /ui/orgs/{org}/agreement/accepted/{id}", u.auth(u.agreementAcceptedPage))
	return nil
}

// agreementPanel is the Agreement section of the organization page.
type agreementPanel struct {
	service.AgreementView
	PendingHTML template.HTML
}

// orgAgreement adds the Agreement section to the organization page's data.
func (u *UI) orgAgreement(r *http.Request, actor service.Actor, org string, data map[string]any) {
	if !u.svc.AgreementRequired() {
		return
	}
	v, err := u.svc.OrgAgreement(r.Context(), actor, org)
	if err != nil {
		u.log.Warn("agreement status", "org", org, "err", err)
		return
	}
	p := agreementPanel{AgreementView: v}
	if v.Pending != nil {
		if p.PendingHTML, err = renderAgreement(v.Pending.Text); err != nil {
			u.log.Warn("render agreement", "version", v.Pending.ID, "err", err)
		}
	}
	data["Agreement"] = p
}

// agreementAcceptPost accepts a version for the organization: a person who
// owns it, from this site, with the box ticked.
func (u *UI) agreementAcceptPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	if err := r.ParseForm(); err != nil {
		u.fail(w, &service.Error{Code: service.CodeInvalid, Msg: "could not read the form: " + err.Error()})
		return
	}
	if !u.checkOrigin(w, r) {
		return
	}
	org := r.PathValue("org")
	var err error
	if actor.Kind != store.KindHuman {
		err = &service.Error{Code: service.CodePermission, Msg: "agents cannot accept the data processing agreement; an owner of " + org + " signs in to the web UI as themselves and accepts it"}
	} else if r.PostForm.Get("agree") != "yes" {
		err = &service.Error{Code: service.CodeInvalid, Msg: "tick the box to say you have read the agreement and accept it, then Accept"}
	} else {
		ctx := r.Context()
		if service.ClientIPFrom(ctx) == "" { // the server's middleware sets it; a bare mux (tests) does not
			host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
			if splitErr != nil {
				host = r.RemoteAddr
			}
			ctx = service.WithClientIP(ctx, host)
		}
		_, err = u.svc.AcceptAgreement(ctx, actor, org, r.PostForm.Get("version"), r.PostForm.Get("sha256"))
	}
	u.orgDone(w, r, actor, err, "agreement", "agreement")
}

// agreementPage shows one published version (?version=, default the one in
// effect); with download=1 it sends the Markdown file.
func (u *UI) agreementPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	org := r.PathValue("org")
	v, err := u.svc.AgreementText(r.Context(), actor, org, param(r, "version"))
	if err != nil {
		u.failPage(w, err)
		return
	}
	if param(r, "download") != "" {
		sendAgreement(w, fmt.Sprintf("dpa-%s-%s.md", org, v.ID), v.Text)
		return
	}
	body, err := renderAgreement(v.Text)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, r, "agreement", map[string]any{
		"Actor": actor, "Title": v.Title, "Section": "orgs", "Org": org, "V": v, "HTML": body,
		"Download": "/ui/orgs/" + url.PathEscape(org) + "/agreement?version=" + url.QueryEscape(v.ID) + "&download=1",
	})
}

// agreementAcceptedPage shows an acceptance with the text exactly as it was
// accepted; with download=1 it sends that text.
func (u *UI) agreementAcceptedPage(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	org := r.PathValue("org")
	rec, text, err := u.svc.AcceptedAgreementText(r.Context(), actor, org, r.PathValue("id"))
	if err != nil {
		u.failPage(w, err)
		return
	}
	if param(r, "download") != "" {
		sendAgreement(w, fmt.Sprintf("dpa-%s-%s-accepted-%s.md", org, rec.VersionID, rec.AcceptedAt.UTC().Format("2006-01-02")), text)
		return
	}
	body, err := renderAgreement(text)
	if err != nil {
		u.fail(w, err)
		return
	}
	title := rec.Title
	if title == "" {
		title = "Data processing agreement, version " + rec.VersionID
	}
	u.render(w, r, "agreement", map[string]any{
		"Actor": actor, "Title": title, "Section": "orgs", "Org": org, "Record": rec, "HTML": body,
		"Download": "/ui/orgs/" + url.PathEscape(org) + "/agreement/accepted/" + rec.ID.String() + "?download=1",
	})
}

// sendAgreement sends an agreement's Markdown as a file download.
func sendAgreement(w http.ResponseWriter, name string, text []byte) {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < ' ' || r == '/' {
			return '-'
		}
		return r
	}, name)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	_, _ = w.Write(text)
}
