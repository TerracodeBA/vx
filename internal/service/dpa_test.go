package service_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// fakeVersion is one version of a test agreement. Its text is obviously
// fake: vx never ships agreement text, the operator provides it.
type fakeVersion struct {
	ID, File, Published, Effective string
	Text                           string // "" means "Test agreement, version ID"
}

// writeAgreements writes a VX_DPA_DIR with a manifest and one file per
// version.
func writeAgreements(t *testing.T, versions ...fakeVersion) string {
	t.Helper()
	dir := t.TempDir()
	var m struct {
		Versions []map[string]string `json:"versions"`
	}
	for _, v := range versions {
		if v.File == "" {
			v.File = v.ID + ".md"
		}
		if v.Text == "" {
			v.Text = "# Test agreement, version " + v.ID + "\n\nThis is **fake** fixture text for tests.\n"
		}
		if v.Text != "-" {
			if err := os.WriteFile(filepath.Join(dir, v.File), []byte(v.Text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		m.Versions = append(m.Versions, map[string]string{"id": v.ID, "file": v.File, "published": v.Published, "effective": v.Effective})
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, service.AgreementManifest), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

func TestLoadAgreementVersions(t *testing.T) {
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	ok := []fakeVersion{
		{ID: "v2", Published: day(now), Effective: day(now.AddDate(0, 0, 30))},
		{ID: "v1", Published: day(now.AddDate(0, -2, 0)), Effective: day(now.AddDate(0, -2, 0))}, // the first may take effect at once
	}
	vs, err := service.LoadAgreementVersions(writeAgreements(t, ok...))
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].ID != "v1" || vs[1].ID != "v2" {
		t.Fatalf("versions = %+v, want v1 then v2 by effective date", vs)
	}
	sum := sha256.Sum256(vs[0].Text)
	if vs[0].SHA256 != hex.EncodeToString(sum[:]) || !strings.Contains(string(vs[0].Text), "Test agreement, version v1") {
		t.Errorf("v1 = %+v", vs[0])
	}
	cases := []struct {
		name     string
		versions []fakeVersion
		want     string
	}{
		{"missing file", []fakeVersion{{ID: "v1", Published: "2026-01-01", Effective: "2026-01-01", Text: "-"}}, "no such file"},
		{"empty file", []fakeVersion{{ID: "v1", Published: "2026-01-01", Effective: "2026-01-01", Text: "  \n"}}, "is empty"},
		{"under 30 days", []fakeVersion{
			{ID: "v1", Published: "2026-01-01", Effective: "2026-01-01"},
			{ID: "v2", Published: "2026-03-01", Effective: "2026-03-30"},
		}, "at least 30 days before"},
		{"duplicate id", []fakeVersion{
			{ID: "v1", Published: "2026-01-01", Effective: "2026-01-01"},
			{ID: "v1", File: "other.md", Published: "2026-01-01", Effective: "2026-06-01"},
		}, "listed twice"},
		{"same effective date", []fakeVersion{
			{ID: "v1", Published: "2026-01-01", Effective: "2026-03-01"},
			{ID: "v2", Published: "2026-01-01", Effective: "2026-03-01"},
		}, "at the same time"},
		{"effective before published", []fakeVersion{{ID: "v1", Published: "2026-02-01", Effective: "2026-01-01"}}, "before it is published"},
		{"bad date", []fakeVersion{{ID: "v1", Published: "soon", Effective: "2026-01-01"}}, "is not a date"},
		{"bad id", []fakeVersion{{ID: "v 1", File: "v1.md", Published: "2026-01-01", Effective: "2026-01-01"}}, "id \"v 1\""},
		{"file outside the directory", []fakeVersion{{ID: "v1", File: "../v1.md", Published: "2026-01-01", Effective: "2026-01-01", Text: "-"}}, "give the name of a file"},
		{"no versions", nil, "lists no versions"},
	}
	for _, c := range cases {
		_, err := service.LoadAgreementVersions(writeAgreements(t, c.versions...))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := service.LoadAgreementVersions(t.TempDir()); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("no manifest: %v", err)
	}
	dir := writeAgreements(t, ok...)
	if err := os.WriteFile(filepath.Join(dir, service.AgreementManifest), []byte(`{"versions":[{"id":"v1","file":"v1.md","published":"2026-01-01","effective":"2026-01-01","signed":true}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoadAgreementVersions(dir); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Errorf("unknown field: %v", err)
	}
}

// dpaEnv is the authz environment (acme with alice, globex with bob)
// with the agreement step on: v1 in effect for 60 days, v2 announced a day
// ago and taking effect in 30 days.
type dpaEnv struct {
	*authzEnv
	clock  time.Time
	v1, v2 service.AgreementVersion
	carol  service.Actor // a member of acme
	dave   service.Actor // an admin of acme
	root   service.Actor // a superadmin outside acme
}

func newDPAEnv(t *testing.T) *dpaEnv {
	t.Helper()
	e := &dpaEnv{authzEnv: newAuthzEnv(t)}
	e.clock = time.Now().UTC()
	e.svc.Now = func() time.Time { return e.clock }
	dir := writeAgreements(t,
		fakeVersion{ID: "v1", Published: day(e.clock.AddDate(0, 0, -60)), Effective: day(e.clock.AddDate(0, 0, -60))},
		fakeVersion{ID: "v2", Published: day(e.clock.AddDate(0, 0, -1)), Effective: day(e.clock.AddDate(0, 0, 30))},
	)
	vs, err := service.LoadAgreementVersions(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.v1, e.v2 = vs[0], vs[1]
	e.svc.Agreement = service.AgreementSettings{Required: true, Dir: dir, Versions: vs}
	if err := e.svc.CheckAgreements(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.carol = e.person("carol", false)
	e.dave = e.person("dave", false)
	e.root = e.person("root", true)
	if _, err := e.svc.SetMember(e.ctx, e.alice, "acme", "carol", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetMember(e.ctx, e.alice, "acme", "dave", service.OrgAdmin); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *dpaEnv) person(handle string, superadmin bool) service.Actor {
	e.t.Helper()
	p, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: handle, SiteAdmin: superadmin})
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	return a
}

func (e *dpaEnv) accept(a service.Actor, org string, v service.AgreementVersion) (service.AgreementRecord, error) {
	return e.svc.AcceptAgreement(service.WithClientIP(e.ctx, "203.0.113.7"), a, org, v.ID, v.SHA256)
}

func isAgreementRefusal(err error, org string) bool {
	return errors.Is(err, service.ErrAgreementRequired) && isCode(err, service.CodeFailedPrecondition) &&
		strings.Contains(err.Error(), "An owner of "+org+" must accept the data processing agreement: Organization > Agreement")
}

// TestAgreementGate: with the step on, an organization that has not
// accepted the version in effect is refused every gated operation with a
// message naming what is missing and its owners; reads keep working; and
// after an owner accepts, work goes on. Off, nothing changes (every other
// test runs with it off).
func TestAgreementGate(t *testing.T) {
	e := newDPAEnv(t)
	ctx := e.ctx
	_, err := e.svc.CreateEffort(ctx, e.agent, service.CreateEffortInput{Repo: "acme/shop", Name: "Blocked"})
	if !isAgreementRefusal(err, "acme") || !strings.Contains(err.Error(), "Owners: alice") || !strings.Contains(err.Error(), "starting efforts") {
		t.Fatalf("CreateEffort without an agreement: %v", err)
	}
	if _, err := e.svc.CreateRepo(ctx, e.alice, service.CreateRepoInput{Name: "acme/new"}); !isAgreementRefusal(err, "acme") {
		t.Errorf("CreateRepo without an agreement: %v", err)
	}
	// Reads, and the status itself, keep working.
	if _, _, err := e.svc.GetRepo(ctx, e.agent, "acme/shop"); err != nil {
		t.Errorf("GetRepo: %v", err)
	}
	if _, err := e.svc.Log(ctx, e.agent, "acme/shop", e.eff.Slug, 10, true); err != nil {
		t.Errorf("Log: %v", err)
	}
	v, err := e.svc.OrgAgreement(ctx, e.agent, "acme")
	if err != nil || v.Usable || v.Pending == nil || v.Pending.ID != "v1" || v.CanAccept || !strings.Contains(v.Missing, "Owners: alice") {
		t.Fatalf("status before = %+v, %v", v, err)
	}
	// Bob cannot learn about acme's agreement: not found, as for any read.
	if _, err := e.svc.OrgAgreement(ctx, e.bob, "acme"); !isCode(err, service.CodeNotFound) {
		t.Errorf("OrgAgreement by an outsider: %v", err)
	}

	rec, err := e.accept(e.alice, "acme", e.v1)
	if err != nil {
		t.Fatal(err)
	}
	if rec.VersionID != "v1" || rec.SHA256 != e.v1.SHA256 || rec.AcceptedByRef != "user:alice" || rec.AcceptedAt.Sub(e.clock).Abs() > time.Millisecond || rec.IP != "203.0.113.7" {
		t.Errorf("acceptance = %+v", rec)
	}
	if again, err := e.accept(e.alice, "acme", e.v1); err != nil || again.ID != rec.ID {
		t.Errorf("accepting twice = %+v, %v; want the first acceptance", again, err)
	}
	ops, err := e.svc.Store.ListTargetOps(ctx, "acme", 20)
	if err != nil {
		t.Fatal(err)
	}
	logged := 0
	for _, op := range ops {
		if op.Op == "org.dpa_accept" {
			logged++
			var after map[string]any
			_ = json.Unmarshal(op.After, &after)
			if after["version"] != "v1" || after["sha256"] != e.v1.SHA256 || after["ip"] != "203.0.113.7" {
				t.Errorf("op log entry = %s", op.After)
			}
		}
	}
	if logged != 1 {
		t.Errorf("org.dpa_accept entries = %d, want 1", logged)
	}
	if _, err := e.svc.CreateEffort(ctx, e.agent, service.CreateEffortInput{Repo: "acme/shop", Name: "Allowed"}); err != nil {
		t.Errorf("CreateEffort after accepting: %v", err)
	}
	// The accepted text, exactly as accepted.
	got, text, err := e.svc.AcceptedAgreementText(ctx, e.carol, "acme", rec.ID.String())
	if err != nil || string(text) != string(e.v1.Text) || got.SHA256 != e.v1.SHA256 {
		t.Errorf("accepted text = %q, %+v, %v", text, got, err)
	}
	// Globex, which accepted nothing, is still refused.
	if _, err := e.svc.CreateEffort(ctx, e.bob, service.CreateEffortInput{Repo: "globex/site", Name: "Blocked"}); !isAgreementRefusal(err, "globex") || !strings.Contains(err.Error(), "Owners: bob") {
		t.Errorf("globex without an agreement: %v", err)
	}
}

// TestAgreementWhoAccepts: only a person who owns the organization accepts.
func TestAgreementWhoAccepts(t *testing.T) {
	e := newDPAEnv(t)
	for _, c := range []struct {
		who  service.Actor
		want service.Code
		msg  string
	}{
		{e.agent, service.CodePermission, "must sign in as themselves"},
		{e.carol, service.CodePermission, "only owners of acme"},
		{e.dave, service.CodePermission, "is admin"},
		{e.root, service.CodePermission, "superadmins cannot accept"},
		{e.bob, service.CodeNotFound, "not found"},
	} {
		if _, err := e.accept(c.who, "acme", e.v1); !isCode(err, c.want) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s accepts: %v, want %q", c.who.Ref(), err, c.msg)
		}
	}
	if v, _ := e.svc.OrgAgreement(e.ctx, e.alice, "acme"); len(v.History) != 0 || !v.CanAccept {
		t.Errorf("after refused acceptances: %+v", v)
	}
	// A superadmin who owns an organization accepts for it like any owner.
	if _, err := e.svc.CreateOrg(e.ctx, e.root, "ops", "Ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.accept(e.root, "ops", e.v1); err != nil {
		t.Errorf("superadmin owner accepts their own organization: %v", err)
	}
	// A changed text and a superseded version are refused.
	if _, err := e.svc.AcceptAgreement(e.ctx, e.alice, "acme", "v1", strings.Repeat("0", 64)); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("wrong hash: %v", err)
	}
	e.clock = e.clock.AddDate(0, 0, 31)
	if _, err := e.accept(e.alice, "acme", e.v1); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "no longer in effect") {
		t.Errorf("superseded version: %v", err)
	}
}

// TestAgreementNewVersion: a new version is announced when published;
// the earlier acceptance counts until it takes effect; from then the
// organization is refused work until an owner accepts it.
func TestAgreementNewVersion(t *testing.T) {
	e := newDPAEnv(t)
	ctx := e.ctx
	if _, err := e.accept(e.alice, "acme", e.v1); err != nil {
		t.Fatal(err)
	}
	notices, err := e.svc.AgreementNotices(ctx, e.alice)
	if err != nil {
		t.Fatal(err)
	}
	var acme *service.AgreementNotice
	for i := range notices {
		if notices[i].Org == "acme" {
			acme = &notices[i]
		}
	}
	if acme == nil || acme.Blocking || acme.Version.ID != "v2" {
		t.Fatalf("notices = %+v, want v2 announced for acme", notices)
	}
	if n, _ := e.svc.AgreementNotices(ctx, e.carol); len(n) != 0 {
		t.Errorf("a member gets notices: %+v", n)
	}
	if n, _ := e.svc.AgreementNotices(ctx, e.agent); len(n) != 0 {
		t.Errorf("an agent gets notices: %+v", n)
	}
	fy, err := e.svc.ForYou(ctx, e.alice)
	if err != nil || fy.Counts[service.ForYouAgreement] == 0 {
		t.Errorf("For you = %+v, %v; want an agreement item", fy.Counts, err)
	}
	v, _ := e.svc.OrgAgreement(ctx, e.alice, "acme")
	if !v.Usable || len(v.Announced) != 1 || v.Pending == nil || v.Pending.ID != "v2" {
		t.Errorf("status with v2 announced = %+v", v)
	}
	// Before v2 takes effect, v1 still counts.
	e.clock = e.clock.AddDate(0, 0, 29)
	if _, err := e.svc.CreateEffort(ctx, e.agent, service.CreateEffortInput{Repo: "acme/shop", Name: "Before v2"}); err != nil {
		t.Errorf("before v2 takes effect: %v", err)
	}
	// From its effective date, the gate applies.
	e.clock = e.clock.AddDate(0, 0, 2)
	_, err = e.svc.OpenWorkspace(ctx, e.agent, "acme/shop", e.eff.Slug, "After v2")
	if !isAgreementRefusal(err, "acme") || !strings.Contains(err.Error(), "version v2 of the data processing agreement took effect") {
		t.Fatalf("after v2 takes effect: %v", err)
	}
	if n, _ := e.svc.AgreementNotices(ctx, e.alice); len(n) == 0 || !n[0].Blocking {
		t.Errorf("notices after v2 took effect = %+v", n)
	}
	if _, err := e.accept(e.alice, "acme", e.v2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OpenWorkspace(ctx, e.agent, "acme/shop", e.eff.Slug, "After accepting v2"); err != nil {
		t.Errorf("after accepting v2: %v", err)
	}
	if v, _ := e.svc.OrgAgreement(ctx, e.carol, "acme"); len(v.History) != 2 || v.History[0].VersionID != "v2" || v.History[1].VersionID != "v1" {
		t.Errorf("history = %+v", v.History)
	}
}

// TestAgreementEarlyAcceptance: an owner may accept an announced version
// before it takes effect; then the organization goes on without a break.
func TestAgreementEarlyAcceptance(t *testing.T) {
	e := newDPAEnv(t)
	for _, v := range []service.AgreementVersion{e.v1, e.v2} {
		if _, err := e.accept(e.alice, "acme", v); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := e.svc.AgreementNotices(e.ctx, e.alice); len(n) != 0 {
		for _, x := range n {
			if x.Org == "acme" {
				t.Errorf("notice after accepting both: %+v", x)
			}
		}
	}
	e.clock = e.clock.AddDate(0, 0, 40)
	if _, err := e.svc.CreateEffort(e.ctx, e.agent, service.CreateEffortInput{Repo: "acme/shop", Name: "No break"}); err != nil {
		t.Errorf("after v2 took effect, accepted early: %v", err)
	}
}

// TestAgreementPersonalOrg: a personal organization is accepted for on
// first use: no notice before it has a repository; its first repository is
// refused with a message saying whose organization it is.
func TestAgreementPersonalOrg(t *testing.T) {
	e := newDPAEnv(t)
	erin, _, _, err := e.svc.AddPerson(e.ctx, nil, service.NewPerson{Handle: "erin", CreateOrg: true})
	if err != nil {
		t.Fatal(err)
	}
	ea, _ := e.svc.ResolveActor(e.ctx, erin)
	if n, _ := e.svc.AgreementNotices(e.ctx, ea); len(n) != 0 {
		t.Errorf("notices for an unused personal organization: %+v", n)
	}
	_, err = e.svc.CreateRepo(e.ctx, ea, service.CreateRepoInput{Name: "erin/tools"})
	if !isAgreementRefusal(err, "erin") || !strings.Contains(err.Error(), "personal organization erin") || !strings.Contains(err.Error(), "Owners: erin") {
		t.Fatalf("first repository of a personal organization: %v", err)
	}
	if _, err := e.accept(ea, "erin", e.v1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateRepo(e.ctx, ea, service.CreateRepoInput{Name: "erin/tools"}); err != nil {
		t.Errorf("after accepting: %v", err)
	}
}

// TestCheckAgreements: the server refuses to start without a version in
// effect, or when a published text changed under an accepted id.
func TestCheckAgreements(t *testing.T) {
	e := newDPAEnv(t)
	if _, err := e.accept(e.alice, "acme", e.v1); err != nil {
		t.Fatal(err)
	}
	changed := e.svc.Agreement
	changed.Versions = append([]service.AgreementVersion(nil), changed.Versions...)
	changed.Versions[0].Text = []byte("Test agreement, version 1, edited")
	sum := sha256.Sum256(changed.Versions[0].Text)
	changed.Versions[0].SHA256 = hex.EncodeToString(sum[:])
	keep := e.svc.Agreement
	e.svc.Agreement = changed
	if err := e.svc.CheckAgreements(e.ctx); err == nil || !strings.Contains(err.Error(), "differs from the one owners accepted") {
		t.Errorf("changed text: %v", err)
	}
	e.svc.Agreement = keep
	future := keep
	future.Versions = []service.AgreementVersion{keep.Versions[1]}
	e.svc.Agreement = future
	if err := e.svc.CheckAgreements(e.ctx); err == nil || !strings.Contains(err.Error(), "no version of the data processing agreement is in effect yet") {
		t.Errorf("nothing in effect: %v", err)
	}
	e.svc.Agreement = service.AgreementSettings{Required: true}
	if err := e.svc.CheckAgreements(context.Background()); err == nil {
		t.Error("required without versions starts")
	}
	e.svc.Agreement = service.AgreementSettings{}
	if err := e.svc.CheckAgreements(e.ctx); err != nil {
		t.Errorf("off: %v", err)
	}
}

// TestAgreementPush: an owner with notifications on gets one push when a
// new version is published, and none again.
func TestAgreementPush(t *testing.T) {
	e := newDPAEnv(t)
	if _, err := e.accept(e.alice, "acme", e.v1); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var topics []string
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		topics = append(topics, r.Header.Get("Topic"))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(push.Close)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e.svc.GitHub.SecretKey = key
	e.svc.Push = service.PushSettings{Client: push.Client(), AllowAnyEndpoint: true}
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	b64 := base64.RawURLEncoding.EncodeToString
	now := e.clock
	e.clock = now.AddDate(0, 0, -2) // notifications on before v2 was published
	if _, err := e.svc.SubscribePush(e.ctx, e.alice, service.PushSubscribeInput{Endpoint: push.URL + "/send/alice", P256dh: b64(ua.PublicKey().Bytes()), Auth: b64([]byte("0123456789abcdef"))}); err != nil {
		t.Fatal(err)
	}
	e.clock = now
	if n, err := e.svc.NotifyPush(e.ctx); err != nil || n != 1 {
		t.Fatalf("NotifyPush = %d, %v", n, err)
	}
	if n, _ := e.svc.NotifyPush(e.ctx); n != 0 {
		t.Errorf("pushed again: %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(topics) != 1 || topics[0] != "dpa-acme" {
		t.Errorf("topics = %v", topics)
	}
}
