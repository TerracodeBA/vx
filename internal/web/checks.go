package web

// Checks (#47 part 3): the Checks tab of the effort page, the checks
// section of the change page, the organization's checks, checks waiting
// for a person on For you, and the artifacts and logs of results. Every
// read goes through the service, which needs read on the repository: an
// artifact is "not found" to anyone else.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/service"
)

func (u *UI) mountChecks(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/checks/artifact", u.auth(u.checkArtifact))
	mux.HandleFunc("GET /ui/checks/log", u.auth(u.checkLog))
	mux.HandleFunc("POST /ui/orgs/{org}/checks", u.auth(u.admin(u.orgChecksPost)))
}

// checkFuncs are the template functions of checks.html.
func checkFuncs() template.FuncMap {
	return template.FuncMap{
		"depth": func(d int) int { return min(d, 6) },
		"checkPill": func(state string) string {
			switch state {
			case rules.StatePassed:
				return "ok"
			case rules.StateFailed:
				return "bad"
			case rules.StateSkipped, rules.StateWaiting, rules.StateReady:
				return "warn"
			}
			return "neutral"
		},
		"trustOf":   func(r *service.CheckResult) string { return service.TrustLabel(r) },
		"checkData": checkData,
		"isImage":   isPreviewImage,
	}
}

type dataField struct{ Key, Value string }

// checkData lists the fields of a result's data (a JSON object), sorted;
// anything else is shown as one value.
func checkData(raw json.RawMessage) []dataField {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return []dataField{{Key: "data", Value: clip(string(raw), 2000)}}
	}
	out := make([]dataField, 0, len(obj))
	for k, v := range obj {
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = string(v)
		}
		out = append(out, dataField{Key: k, Value: clip(s, 500)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// previewImages are the artifact types the pages show inline. SVG is not
// one of them: it can carry script, so it is always a download.
var previewImages = map[string]bool{"image/png": true, "image/gif": true, "image/jpeg": true, "image/webp": true}

func isPreviewImage(contentType string) bool {
	ct, _, _ := mime.ParseMediaType(contentType)
	return previewImages[strings.ToLower(ct)]
}

// pipeline is the checks of a change or effort head for a page; nil when
// they cannot be read (the page still renders).
func (u *UI) pipeline(r *http.Request, actor service.Actor, in service.CheckStatusInput) *service.PipelineView {
	v, err := u.svc.PipelineStatus(r.Context(), actor, in)
	if err != nil {
		u.log.Warn("checks", "repo", in.Repo, "effort", in.Effort, "change", in.ChangeID, "err", err)
		return nil
	}
	return &v
}

// checkArtifact serves one artifact of a result: images the pages preview
// inline, everything else (and anything with download=1) as a download,
// never sniffed and under a CSP that runs nothing.
func (u *UI) checkArtifact(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	id, name := param(r, "result"), param(r, "name")
	if _, err := uuid.Parse(id); err != nil || name == "" {
		u.fail(w, &service.Error{Code: service.CodeNotFound, Msg: "no such check result or artifact"})
		return
	}
	a, data, err := u.svc.CheckArtifact(r.Context(), actor, id, name)
	if err != nil {
		u.fail(w, err)
		return
	}
	disposition := "attachment"
	ct := "application/octet-stream"
	if isPreviewImage(a.ContentType) {
		ct = a.ContentType
		if param(r, "download") == "" {
			disposition = "inline"
		}
	}
	serveCheckFile(w, ct, disposition, a.Name, data)
}

// checkLog serves the log of a result as plain text.
func (u *UI) checkLog(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	id := param(r, "result")
	if _, err := uuid.Parse(id); err != nil {
		u.fail(w, &service.Error{Code: service.CodeNotFound, Msg: "no such check result"})
		return
	}
	_, data, err := u.svc.CheckArtifact(r.Context(), actor, id, "")
	if err != nil {
		u.fail(w, err)
		return
	}
	serveCheckFile(w, "text/plain; charset=utf-8", "inline", "check-"+id[:8]+".log", data)
}

func serveCheckFile(w http.ResponseWriter, contentType, disposition, name string, data []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

// orgChecksPost saves a new version of the organization's checks.
func (u *UI) orgChecksPost(w http.ResponseWriter, r *http.Request, actor service.Actor) {
	src := strings.ReplaceAll(r.PostForm.Get("source"), "\r\n", "\n")
	if strings.TrimSpace(src) == "" {
		src = ""
	}
	_, _, err := u.svc.SetOrgChecks(r.Context(), actor, r.PathValue("org"), src)
	u.orgDone(w, r, actor, err, "checks", "checks")
}

// orgChecks adds the organization's checks and their history to the
// organization page's data.
func (u *UI) orgChecks(r *http.Request, actor service.Actor, org string, data map[string]any) {
	cur, err := u.svc.GetOrgChecks(r.Context(), actor, org)
	if err != nil {
		u.log.Warn("org checks", "org", org, "err", err)
		return
	}
	data["OrgChecks"] = cur
	if hist, err := u.svc.OrgCheckHistory(r.Context(), actor, org); err == nil {
		data["CheckHistory"] = hist
	}
}

// humanChecks are the checks waiting for the person on For you; nil when
// they cannot be listed.
func (u *UI) humanChecks(r *http.Request, actor service.Actor) []service.HumanCheck {
	hs, err := u.svc.HumanChecksFor(r.Context(), actor)
	if err != nil {
		u.log.Warn("checks for you", "err", err)
		return nil
	}
	return hs
}
