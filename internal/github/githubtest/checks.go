package githubtest

// Check runs and the App's permissions: POST /repos/{owner}/{repo}/check-runs
// needs the App permission checks: write, as on GitHub. An App created by
// the manifest flow has the manifest's permissions and events; one made
// with NewApp has the ones vx asks for. SetAppPermission takes one away,
// as with an App created before vx asked for it.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

// CheckRun is a check run posted to the fake.
type CheckRun struct {
	ID         int64
	Repo       string // owner/name
	Name       string
	HeadSHA    string
	Status     string
	Conclusion string
	Title      string
	Summary    string
	Text       string
	DetailsURL string
	ExternalID string
	At         time.Time
	Updates    int // PATCHes since it was created
}

// appPermissionsLocked returns the App's permissions.
func (s *Server) appPermissionsLocked() map[string]string {
	if s.appPerms == nil {
		s.appPerms = map[string]string{}
		if m, ok := s.manifest["default_permissions"].(map[string]any); ok {
			for k, v := range m {
				if str, ok := v.(string); ok {
					s.appPerms[k] = str
				}
			}
		} else {
			for k, v := range service.GitHubAppPermissions {
				s.appPerms[k] = v
			}
		}
	}
	return s.appPerms
}

func (s *Server) appEventsLocked() []string {
	if s.appEvents == nil {
		if list, ok := s.manifest["default_events"].([]any); ok {
			for _, v := range list {
				if str, ok := v.(string); ok {
					s.appEvents = append(s.appEvents, str)
				}
			}
		} else {
			s.appEvents = append([]string{}, service.GitHubAppEvents...)
		}
	}
	return s.appEvents
}

// SetAppPermission sets one of the App's permissions ("" removes it).
func (s *Server) SetAppPermission(name, level string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	perms := s.appPermissionsLocked()
	if level == "" {
		delete(perms, name)
		return
	}
	perms[name] = level
}

// SetAppEvents replaces the App's webhook events.
func (s *Server) SetAppEvents(events ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appEvents = append([]string{}, events...)
}

// CheckRun returns the check run with this id as it is now.
func (s *Server) CheckRun(id int64) (CheckRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checkRuns {
		if c.ID == id {
			return c, true
		}
	}
	return CheckRun{}, false
}

// CheckRuns returns the check runs posted on fullName, oldest first.
func (s *Server) CheckRuns(fullName string) []CheckRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CheckRun
	for _, c := range s.checkRuns {
		if strings.EqualFold(c.Repo, fullName) {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) createCheckRun(w http.ResponseWriter, r *http.Request, owner, name string) {
	in, ok := s.installationFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		DetailsURL string `json:"details_url"`
		ExternalID string `json:"external_id"`
		Output     struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
			Text    string `json:"text"`
		} `json:"output"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.HeadSHA == "" {
		writeMsg(w, http.StatusUnprocessableEntity, "Invalid request: name and head_sha are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repoInInstallation(in, owner, name)
	if rp == nil {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	if s.appPermissionsLocked()["checks"] != "write" {
		writeMsg(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	c := CheckRun{
		ID: s.id(), Repo: rp.FullName(), Name: body.Name, HeadSHA: body.HeadSHA, Status: body.Status, Conclusion: body.Conclusion,
		Title: body.Output.Title, Summary: body.Output.Summary, Text: body.Output.Text, DetailsURL: body.DetailsURL, ExternalID: body.ExternalID, At: s.Now(),
	}
	s.checkRuns = append(s.checkRuns, c)
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "name": c.Name, "head_sha": c.HeadSHA, "status": c.Status, "conclusion": c.Conclusion})
}

// updateCheckRun is PATCH /repos/{owner}/{repo}/check-runs/{id}: fields
// sent replace the run's; a completed run keeps its conclusion unless a
// new one is sent.
func (s *Server) updateCheckRun(w http.ResponseWriter, r *http.Request, owner, name, idStr string) {
	in, ok := s.installationFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Name       *string `json:"name"`
		Status     *string `json:"status"`
		Conclusion *string `json:"conclusion"`
		DetailsURL *string `json:"details_url"`
		ExternalID *string `json:"external_id"`
		Output     *struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
			Text    string `json:"text"`
		} `json:"output"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeMsg(w, http.StatusUnprocessableEntity, "Invalid request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repoInInstallation(in, owner, name)
	if rp == nil {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	if s.appPermissionsLocked()["checks"] != "write" {
		writeMsg(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	for i := range s.checkRuns {
		c := &s.checkRuns[i]
		if strconv.FormatInt(c.ID, 10) != idStr || !strings.EqualFold(c.Repo, rp.FullName()) {
			continue
		}
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = *v
			}
		}
		set(&c.Name, body.Name)
		set(&c.Status, body.Status)
		set(&c.Conclusion, body.Conclusion)
		set(&c.DetailsURL, body.DetailsURL)
		set(&c.ExternalID, body.ExternalID)
		if body.Output != nil {
			c.Title, c.Summary, c.Text = body.Output.Title, body.Output.Summary, body.Output.Text
		}
		c.Updates++
		c.At = s.Now()
		writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "name": c.Name, "head_sha": c.HeadSHA, "status": c.Status, "conclusion": c.Conclusion})
		return
	}
	writeMsg(w, http.StatusNotFound, "Not Found")
}
