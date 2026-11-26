package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The Delete organization section is for owners; a deleted organization
// shows its owners a page to restore it and download the confirmation, and
// is a 404 for everyone else; its repositories are 404 for all.
func TestOrgDeletePages(t *testing.T) {
	e := newAdminEnv(t)
	if _, body := e.get("alice", "/ui/orgs/acme"); !strings.Contains(body, "Delete organization") || !strings.Contains(body, "uninstall it on GitHub") {
		t.Fatal("the owner's organization page has no Delete organization section")
	}
	if _, body := e.get("bob", "/ui/orgs/acme"); strings.Contains(body, "Delete organization") {
		t.Fatal("a member sees Delete organization")
	}
	if _, body := e.get("alice", "/ui/orgs/alice"); strings.Contains(body, "Delete organization") {
		t.Fatal("a personal organization offers Delete organization")
	}
	if resp, body := e.post("bob", "/ui/orgs/acme/delete", url.Values{"confirm": {"acme"}}, ""); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "needs owner") {
		t.Fatalf("a member deletes: %d\n%s", resp.StatusCode, body)
	}
	if resp, body := e.post("agent", "/ui/orgs/acme/delete", url.Values{"confirm": {"acme"}}, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an agent deletes: %d\n%s", resp.StatusCode, body)
	}
	if resp, body := e.post("alice", "/ui/orgs/acme/delete", url.Values{"confirm": {"acm"}}, ""); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "type its handle") {
		t.Fatalf("deleting with a wrong confirmation: %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := e.post("alice", "/ui/orgs/acme/delete", url.Values{"confirm": {"acme"}}, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a delete posted from another site: %d", resp.StatusCode)
	}
	resp, body := e.post("alice", "/ui/orgs/acme/delete", url.Values{"confirm": {"acme"}}, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Organization deleted") || !strings.Contains(body, "Restore organization") || !strings.Contains(body, "scheduled for") {
		t.Fatalf("after deleting: %d\n%s", resp.StatusCode, body)
	}
	for _, who := range []string{"bob", "carol"} {
		if status, _ := e.get(who, "/ui/orgs/acme"); status != http.StatusNotFound {
			t.Errorf("%s opens the deleted organization: %d", who, status)
		}
		if status, _ := e.get(who, "/ui/orgs/acme/deletion-record"); status != http.StatusNotFound {
			t.Errorf("%s downloads the deletion record: %d", who, status)
		}
	}
	for _, who := range []string{"alice", "bob"} {
		if status, _ := e.get(who, "/ui/repo?name=acme/shop"); status != http.StatusNotFound {
			t.Errorf("%s opens acme/shop after the deletion: %d", who, status)
		}
		if _, body := e.get(who, "/ui/orgs"); strings.Contains(body, `href="/ui/orgs/acme">Acme`) {
			t.Errorf("%s still lists acme among their organizations", who)
		}
	}
	if _, body := e.get("alice", "/ui/orgs"); !strings.Contains(body, "Deleted organizations") {
		t.Error("the owner's organizations page does not list the deleted organization")
	}
	r, err := e.browsers["alice"].Get(e.srv.URL + "/ui/orgs/acme/deletion-record")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") || !strings.Contains(r.Header.Get("Content-Disposition"), "vx-deletion-acme.txt") {
		t.Fatalf("deletion record download: %d %v", r.StatusCode, r.Header)
	}
	resp, body = e.post("alice", "/ui/orgs/acme/restore", nil, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Organization restored") {
		t.Fatalf("restore: %d\n%s", resp.StatusCode, body)
	}
	if status, _ := e.get("bob", "/ui/repo?name=acme/shop"); status != 200 {
		t.Errorf("bob opens acme/shop after the restore: %d", status)
	}
}
