package service_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// supportEnv is the authz environment plus root, a superadmin in no
// organization, and root's agent.
func newSupportEnv(t *testing.T) (*authzEnv, service.Actor, service.Actor) {
	t.Helper()
	e := newAuthzEnv(t)
	root, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: "root", SiteAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindAgent, Handle: "root/ops", ParentID: &root.ID})
	if err != nil {
		t.Fatal(err)
	}
	sa, _ := e.svc.ResolveActor(e.ctx, root)
	saAgent, _ := e.svc.ResolveActor(e.ctx, ag)
	return e, sa, saAgent
}

// TestSupportGrantRules: who may grant support access, to whom, what role
// and for how long; every grant and revocation is in the operation log.
func TestSupportGrantRules(t *testing.T) {
	e, root, rootAgent := newSupportEnv(t)
	in := func(mut func(*service.SupportInput)) service.SupportInput {
		v := service.SupportInput{Repo: "acme/shop", Person: "root", Role: service.RoleRead, For: 24 * time.Hour, Reason: "ticket 4711"}
		if mut != nil {
			mut(&v)
		}
		return v
	}
	cases := []struct {
		name  string
		actor service.Actor
		in    service.SupportInput
		code  service.Code
		parts []string
	}{
		{"admin role", e.alice, in(func(v *service.SupportInput) { v.Role = service.RoleAdmin }), service.CodeInvalid, []string{"read or write", "never includes admin"}},
		{"too long", e.alice, in(func(v *service.SupportInput) { v.For = 31 * 24 * time.Hour }), service.CodeInvalid, []string{"out of range"}},
		{"too short", e.alice, in(func(v *service.SupportInput) { v.For = time.Second }), service.CodeInvalid, []string{"out of range"}},
		{"not a superadmin", e.alice, in(func(v *service.SupportInput) { v.Person = "bob" }), service.CodeFailedPrecondition, []string{"user:bob is not a superadmin", "vx repo grant acme/shop user:bob"}},
		{"an agent", e.alice, in(func(v *service.SupportInput) { v.Person = "root/ops" }), service.CodeInvalid, []string{"not a person"}},
		{"by an agent", e.agent, in(nil), service.CodePermission, []string{"only people can grant support access"}},
		{"by a non-reader", e.bob, in(nil), service.CodeNotFound, nil},
		{"by the superadmin", root, in(nil), service.CodeNotFound, nil},
		{"by the superadmin's agent", rootAgent, in(nil), service.CodePermission, nil},
	}
	for _, c := range cases {
		_, err := e.svc.GrantSupport(e.ctx, c.actor, c.in)
		wantErr(t, err, c.code, c.parts...)
		if t.Failed() {
			t.Fatalf("case %q", c.name)
		}
	}
	// A reader cannot grant it either: it needs admin on the repository.
	e.grant(service.RoleWrite)
	_, err := e.svc.GrantSupport(e.ctx, e.bob, in(nil))
	wantErr(t, err, service.CodePermission, "needs admin")

	g, err := e.svc.GrantSupport(e.ctx, e.alice, in(func(v *service.SupportInput) { v.For = 0 }))
	if err != nil {
		t.Fatal(err)
	}
	if g.Role != service.RoleRead || g.GrantedBy != "user:alice" || g.Reason != "ticket 4711" ||
		g.ExpiresAt.Before(time.Now().Add(23*time.Hour)) || g.ExpiresAt.After(time.Now().Add(25*time.Hour)) {
		t.Fatalf("grant = %+v", g)
	}
	list, err := e.svc.ListSupport(e.ctx, e.alice, "acme/shop")
	if err != nil || len(list) != 1 || list[0].Person.Handle != "root" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, err := e.svc.ListSupport(e.ctx, root, "acme/shop"); !isCode(err, service.CodePermission) {
		t.Errorf("the superadmin lists support access with read: %v", err)
	}
	// Granting again replaces it: write for an hour.
	if _, err := e.svc.GrantSupport(e.ctx, e.alice, in(func(v *service.SupportInput) { v.Role, v.For = service.RoleWrite, time.Hour })); err != nil {
		t.Fatal(err)
	}
	if _, role, err := e.svc.Authorize(e.ctx, rootAgent, "acme/shop", service.RoleWrite); err != nil || role != service.RoleWrite {
		t.Errorf("superadmin's agent with write support access: %v %v", role, err)
	}
	// The superadmin ends their own; nobody else without admin can.
	wantErr(t, e.svc.RevokeSupport(e.ctx, e.bob, "acme/shop", "root"), service.CodePermission, "needs admin")
	wantErr(t, e.svc.RevokeSupport(e.ctx, rootAgent, "acme/shop", "root"), service.CodePermission, "only people")
	if err := e.svc.RevokeSupport(e.ctx, root, "acme/shop", "root"); err != nil {
		t.Fatalf("superadmin ends their own: %v", err)
	}
	if _, _, err := e.svc.Authorize(e.ctx, root, "acme/shop", service.RoleRead); !isCode(err, service.CodeNotFound) {
		t.Errorf("after ending it: %v", err)
	}
	wantErr(t, e.svc.RevokeSupport(e.ctx, e.alice, "acme/shop", "root"), service.CodeNotFound, "user:root has no support access to acme/shop")

	ops, err := e.svc.Store.ListOps(e.ctx, e.repo.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range ops {
		if strings.HasPrefix(o.Op, "repo.support.") {
			got = append(got, o.Op+" "+string(o.After)+" "+string(o.Before))
		}
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], "repo.support.revoke") || !strings.HasPrefix(got[2], "repo.support.grant") ||
		!strings.Contains(got[2], `"person": "user:root"`) || !strings.Contains(got[2], `"reason": "ticket 4711"`) || !strings.Contains(got[2], `"expires_at": `) ||
		!strings.Contains(got[1], `"role": "write"`) || !strings.Contains(got[1], `"role": "read"`) {
		t.Errorf("operation log:\n%s", strings.Join(got, "\n"))
	}
}

// TestSupportEndsWithSuperadmin: a support grant counts only while its
// person is a superadmin, and only until it expires; the access list shows
// it while it counts.
func TestSupportEndsWithSuperadmin(t *testing.T) {
	e, root, _ := newSupportEnv(t)
	if _, err := e.svc.GrantSupport(e.ctx, e.alice, service.SupportInput{Repo: "acme/shop", Person: "root", For: 2 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	_, entries, err := e.svc.RepoAccess(e.ctx, e.alice, "acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	var via []string
	for _, en := range entries {
		if en.Person.Handle == "root" {
			via = en.Via
		}
	}
	if len(via) != 1 || !strings.HasPrefix(via[0], "operator (support access) until ") || !strings.HasSuffix(via[0], " UTC") {
		t.Errorf("access entry = %v", via)
	}
	if err := e.svc.Store.SetSiteAdmin(e.ctx, root.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Authorize(e.ctx, root, "acme/shop", service.RoleRead); !isCode(err, service.CodeNotFound) {
		t.Errorf("no longer a superadmin: %v", err)
	}
	if ids, _, _ := e.svc.ReadableRepoIDs(e.ctx, root); len(ids) != 0 {
		t.Errorf("no longer a superadmin: readable %v", ids)
	}
	if err := e.svc.Store.SetSiteAdmin(e.ctx, root.ID, true); err != nil {
		t.Fatal(err)
	}
	if ids, _, _ := e.svc.ReadableRepoIDs(e.ctx, root); len(ids) != 1 || ids[0] != e.repo.ID {
		t.Errorf("superadmin again: readable %v", ids)
	}
	clock := e.svc.Now
	defer func() { e.svc.Now = clock }()
	e.svc.Now = func() time.Time { return clock().Add(3 * time.Hour) }
	if _, _, err := e.svc.Authorize(e.ctx, root, "acme/shop", service.RoleRead); !isCode(err, service.CodeNotFound) {
		t.Errorf("expired: %v", err)
	}
	if list, err := e.svc.ListSupport(e.ctx, e.alice, "acme/shop"); err != nil || len(list) != 0 {
		t.Errorf("expired grant listed: %+v %v", list, err)
	}
}

// TestSuperadminObjectsAndLists: a superadmin cannot download a
// repository's objects, see its questions in the inbox, its reviews, or
// change organizations to give themselves access; the admin panel works.
func TestSuperadminObjectsAndLists(t *testing.T) {
	e, root, rootAgent := newSupportEnv(t)
	row, err := e.svc.Store.GetChangeset(e.ctx, e.repo.ID, e.eff.Base)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []service.Actor{root, rootAgent} {
		if _, err := e.svc.GetObjects(e.ctx, a, "acme/shop", []hash.Hash{row.Tree}, 0); !isCode(err, service.CodeNotFound) {
			t.Errorf("%s downloads objects: %v", a.Ref(), err)
		}
		if _, err := e.svc.MissingObjects(e.ctx, a, "acme/shop", []hash.Hash{row.Tree}); !isCode(err, service.CodeNotFound) {
			t.Errorf("%s asks which objects exist: %v", a.Ref(), err)
		}
		if _, err := e.svc.PutObjects(e.ctx, a, "acme/shop", nil); !isCode(err, service.CodeNotFound) {
			t.Errorf("%s uploads objects: %v", a.Ref(), err)
		}
	}
	if _, err := e.svc.Ask(e.ctx, e.agent, service.AskInput{Repo: "acme/shop", Effort: e.eff.Slug, Targets: []string{"cart.go"}, Body: "Operator, can you see this?", To: "user:root"}); err != nil {
		t.Fatal(err)
	}
	if in, err := e.svc.GetInbox(e.ctx, root); err != nil || len(in.Questions) != 0 {
		t.Errorf("superadmin's inbox = %+v %v", in.Questions, err)
	}
	if rs, err := e.svc.ListReviews(e.ctx, root, service.ReviewQuery{}); err != nil || len(rs) != 0 {
		t.Errorf("superadmin's reviews = %+v %v", rs, err)
	}
	// Organizations: visible, never changed by a superadmin who is not an
	// owner or admin there.
	d, err := e.svc.GetOrg(e.ctx, root, "acme")
	if err != nil || len(d.Repos) != 0 || d.MyRole != "" {
		t.Fatalf("superadmin GetOrg acme = %+v %v", d, err)
	}
	_, err = e.svc.SetMember(e.ctx, root, "acme", "root", service.OrgOwner)
	wantErr(t, err, service.CodePermission, "is a superadmin, which lets them see organizations but not change them")
	_, err = e.svc.CreateTeam(e.ctx, root, "acme", "ops")
	wantErr(t, err, service.CodePermission, "superadmin")
	_, err = e.svc.UpdateOrg(e.ctx, root, "acme", "Pwned", nil)
	wantErr(t, err, service.CodePermission, "superadmin")
	_, err = e.svc.CreateRepo(e.ctx, root, service.CreateRepoInput{Name: "acme/backdoor", Org: "acme"})
	wantErr(t, err, service.CodePermission, "superadmin")
	_, err = e.svc.GrantRepo(e.ctx, root, "acme/shop", "user:root", service.RoleAdmin)
	wantErr(t, err, service.CodeNotFound)
	// The admin panel: people, every organization, creating people.
	if people, err := e.svc.ListPeople(e.ctx, root); err != nil || len(people) != 3 {
		t.Errorf("ListPeople = %d %v", len(people), err)
	}
	if orgs, _, err := e.svc.ListOrgs(e.ctx, root, true); err != nil || len(orgs) < 2 {
		t.Errorf("ListOrgs all = %d %v", len(orgs), err)
	}
	if _, _, err := e.svc.CreatePerson(e.ctx, root, "dora", "Dora", "", false, "acme"); err != nil {
		t.Errorf("CreatePerson into acme: %v", err)
	}
	if _, _, err := e.svc.Authorize(e.ctx, root, "acme/shop", service.RoleRead); !isCode(err, service.CodeNotFound) {
		t.Errorf("adding someone to acme gave the superadmin access: %v", err)
	}
	// A customer may still give a superadmin an ordinary role explicitly.
	if _, err := e.svc.GrantRepo(e.ctx, e.alice, "acme/shop", "user:root", service.RoleRead); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Authorize(e.ctx, rootAgent, "acme/shop", service.RoleRead); err != nil {
		t.Errorf("explicit grant to a superadmin: %v", err)
	}
}

// TestUpgradeNoticeLoggedOnce: the upgrade review is logged on the first
// start only, shown to superadmins until one resolves it, and resolving it
// is in the operation log.
func TestUpgradeNoticeLoggedOnce(t *testing.T) {
	e, root, rootAgent := newSupportEnv(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	if err := e.svc.LogUpgradeNotices(context.Background(), logger); err != nil || buf.Len() != 0 {
		t.Fatalf("a new server logs %q %v", buf.String(), err)
	}
	if e.svc.SuperadminReviewPending(e.ctx, root) {
		t.Error("a new server asks for the review")
	}
	if err := e.svc.Store.AddServerNotice(e.ctx, service.NoticeSuperadminReview); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.svc.LogUpgradeNotices(context.Background(), logger); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "no longer have access to repositories"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, buf.String())
	}
	if !e.svc.SuperadminReviewPending(e.ctx, root) || e.svc.SuperadminReviewPending(e.ctx, e.bob) {
		t.Error("the review shows to superadmins only")
	}
	wantErr(t, e.svc.ResolveSuperadminReview(e.ctx, rootAgent), service.CodePermission)
	wantErr(t, e.svc.ResolveSuperadminReview(e.ctx, e.bob), service.CodePermission)
	if err := e.svc.ResolveSuperadminReview(e.ctx, root); err != nil {
		t.Fatal(err)
	}
	if e.svc.SuperadminReviewPending(e.ctx, root) {
		t.Error("still pending after the review")
	}
	ops, err := e.svc.Store.ListActorOps(e.ctx, root.ID, 5)
	if err != nil || len(ops) != 1 || ops[0].Op != "server.notice.resolve" || ops[0].Target != service.NoticeSuperadminReview {
		t.Errorf("op log = %+v %v", ops, err)
	}
}
