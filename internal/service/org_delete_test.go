package service_test

// Deleting and purging organizations (#40).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func (e *authzEnv) person(handle string, superadmin bool) service.Actor {
	e.t.Helper()
	p, err := e.svc.Store.CreatePrincipal(e.ctx, store.Principal{Kind: store.KindHuman, Handle: handle, DisplayName: handle, SiteAdmin: superadmin})
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ := e.svc.ResolveActor(e.ctx, p)
	return a
}

// later moves the service's clock past the grace period of a deletion.
func (e *authzEnv) later(d time.Duration) {
	clock := e.svc.Now
	e.svc.Now = func() time.Time { return clock().Add(d) }
}

func TestDeleteOrgOwnersOnly(t *testing.T) {
	e := newAuthzEnv(t)
	carol, dave := e.person("carol", false), e.person("dave", false)
	root := e.person("root", true)
	for _, m := range []struct {
		who  string
		role service.OrgRole
	}{{"bob", service.OrgAdmin}, {"carol", service.OrgMember}} {
		if _, err := e.svc.SetMember(e.ctx, e.alice, "acme", m.who, m.role); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		who  service.Actor
		why  string
		want service.Code
	}{
		{e.bob, "an admin", service.CodePermission},
		{carol, "a member", service.CodePermission},
		{e.agent, "the owner's agent", service.CodePermission},
		{root, "a superadmin", service.CodePermission},
		{dave, "not a member", service.CodeNotFound},
	} {
		if _, err := e.svc.DeleteOrg(e.ctx, c.who, "acme", "acme"); !isCode(err, c.want) {
			t.Errorf("%s deletes acme: want code %v, got %v", c.why, c.want, err)
		}
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "alice", "alice"); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "personal") {
		t.Errorf("deleting a personal organization: %v", err)
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acm"); !isCode(err, service.CodeInvalid) || !strings.Contains(err.Error(), "--confirm acme") {
		t.Errorf("deleting without the handle confirmed: %v", err)
	}
	d, err := e.svc.DeleteOrg(e.ctx, e.alice, "ACME", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if d.Org != "acme" || d.RequestedBy != "user:alice" || d.PurgeAfter.Sub(d.DeletedAt) != service.DefaultOrgDeleteGrace {
		t.Errorf("deletion = %+v", d)
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); !isCode(err, service.CodeNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	// Only owners see the deleted organization and restore it.
	for _, a := range []service.Actor{e.bob, carol, root, dave} {
		if _, err := e.svc.OrgDeletionRecord(e.ctx, a, "acme"); !isCode(err, service.CodeNotFound) {
			t.Errorf("%s reads the deletion record: %v", a.Ref(), err)
		}
		if _, err := e.svc.RestoreOrg(e.ctx, a, "acme"); !isCode(err, service.CodeNotFound) {
			t.Errorf("%s restores: %v", a.Ref(), err)
		}
	}
	if _, err := e.svc.RestoreOrg(e.ctx, e.agent, "acme"); !isCode(err, service.CodePermission) {
		t.Errorf("an agent restores: %v", err)
	}
}

// deletedRows extends TestAuthzMatrix: once acme is deleted, every
// repository operation on acme/shop is "not found" for everyone, its
// owner included, until it is restored.
func (e *authzEnv) deletedRows(t *testing.T) {
	t.Helper()
	e.grant(service.RoleAdmin)
	e.frozenReview = e.pendingReview()
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []service.Actor{e.alice, e.agent, e.bob, e.bobAgent} {
		for _, op := range authzOps {
			if err := op.run(e, actor); !isCode(err, service.CodeNotFound) {
				t.Errorf("%s by %s after acme was deleted: want not found, got %v", op.name, actor.Ref(), err)
			}
		}
		repos, err := e.svc.ListRepos(e.ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range repos {
			if r.Org == "acme" {
				t.Errorf("%s still lists %s", actor.Ref(), r.Name)
			}
		}
		orgs, _, err := e.svc.ListOrgs(e.ctx, actor, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range orgs {
			if o.Handle == "acme" {
				t.Errorf("%s still lists organization acme", actor.Ref())
			}
		}
		if _, err := e.svc.GetOrg(e.ctx, actor, "acme"); !isCode(err, service.CodeNotFound) {
			t.Errorf("GetOrg acme by %s: %v", actor.Ref(), err)
		}
	}
	// The handle stays reserved.
	if _, err := e.svc.CreateOrg(e.ctx, e.bob, "acme", ""); !isCode(err, service.CodeExists) {
		t.Errorf("creating an organization with a deleted one's handle: %v", err)
	}
	if _, err := e.svc.RestoreOrg(e.ctx, e.alice, "acme"); err != nil {
		t.Fatal(err)
	}
	e.frozenReview = ""
	for _, actor := range []service.Actor{e.alice, e.agent, e.bob, e.bobAgent} {
		for _, op := range authzOps {
			if err := op.run(e, actor); err != nil {
				t.Errorf("%s by %s after acme was restored: %v", op.name, actor.Ref(), err)
			}
		}
	}
}

func TestRestoreOrg(t *testing.T) {
	e := newAuthzEnv(t)
	if _, err := e.svc.SetMember(e.ctx, e.alice, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	before, err := e.svc.GetOrg(e.ctx, e.alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	dels, err := e.svc.DeletedOrgs(e.ctx, e.alice)
	if err != nil || len(dels) != 1 || dels[0].Org != "acme" {
		t.Fatalf("DeletedOrgs = %+v, %v", dels, err)
	}
	d, err := e.svc.OrgDeletionRecord(e.ctx, e.alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	text := d.Confirmation("https://vx.example.com")
	for _, want := range []string{"Organization:    acme", "Deleted by:      user:alice", "scheduled for", "vx org restore acme"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "shop") {
		t.Errorf("the confirmation names a repository:\n%s", text)
	}
	if _, err := e.svc.RestoreOrg(e.ctx, e.alice, "acme"); err != nil {
		t.Fatal(err)
	}
	after, err := e.svc.GetOrg(e.ctx, e.alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Members) != len(before.Members) || len(after.Repos) != len(before.Repos) || after.Org != before.Org {
		t.Errorf("restored organization differs:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, err := e.svc.GetEffort(e.ctx, e.bob, "acme/shop", e.eff.Slug); err != nil {
		t.Errorf("bob after the restore: %v", err)
	}
	if dels, _ := e.svc.DeletedOrgs(e.ctx, e.alice); len(dels) != 0 {
		t.Errorf("still listed as deleted: %+v", dels)
	}
	ops := e.ops("acme")
	if !strings.Contains(ops, "org.delete") || !strings.Contains(ops, "org.restore") {
		t.Errorf("operation log: %s", ops)
	}
	// After the grace period it can no longer be restored.
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	e.later(service.DefaultOrgDeleteGrace + time.Minute)
	if _, err := e.svc.RestoreOrg(e.ctx, e.alice, "acme"); !isCode(err, service.CodeFailedPrecondition) {
		t.Errorf("restore after the grace period: %v", err)
	}
}

// ops lists the operation log entries about an organization, as op names.
func (e *authzEnv) ops(org string) string {
	e.t.Helper()
	rows, err := e.svc.Store.ListAudit(e.ctx, store.AuditFilter{OrgHandle: org, Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.Org == org {
			out = append(out, r.Op)
		}
	}
	return strings.Join(out, " ")
}

// fillOrg puts a bit of everything into acme: members and a team, a
// grant, quota and support access, a task, a claim, a reservation, a
// review, a push marker, a GitHub installation link, and an object shared
// with globex/site.
func (e *authzEnv) fillOrg() (shared hash.Hash) {
	t := e.t
	t.Helper()
	ctx := e.ctx
	if _, err := e.svc.SetMember(ctx, e.alice, "acme", "bob", service.OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateTeam(ctx, e.alice, "acme", "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetTeamMember(ctx, e.alice, "acme", "backend", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GrantRepo(ctx, e.alice, "acme/shop", "team:acme/backend", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	e.person("root", true)
	if _, err := e.svc.GrantSupport(ctx, e.alice, service.SupportInput{Repo: "acme/shop", Person: "root", For: time.Hour}); err != nil {
		t.Fatal(err)
	}
	repos := int64(5)
	if err := e.svc.Store.SetOrgQuota(ctx, store.OrgQuota{OrgID: e.repo.OrgID, Repos: &repos}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddTask(ctx, e.agent, service.AddTaskInput{Repo: "acme/shop", Effort: e.eff.Slug, Title: "Totals"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.MakeClaim(ctx, e.agent, service.ClaimInput{Repo: "acme/shop", Effort: e.eff.Slug, Targets: []string{"cart.go"}}); err != nil {
		t.Fatal(err)
	}
	e.pendingReview()
	if _, err := e.svc.Store.ClaimPush(ctx, e.bob.ID, "question:"+e.question.ID.String(), time.Now()); err != nil {
		t.Fatal(err)
	}
	gh := int64(4242)
	if err := e.svc.Store.SetOrgGitHub(ctx, e.repo.OrgID, &gh, "acme-gh"); err != nil {
		t.Fatal(err)
	}
	orgID := e.repo.OrgID
	if err := e.svc.Store.UpsertGitHubInstallation(ctx, store.GitHubInstallationRow{ID: 77, AccountID: gh, AccountLogin: "acme-gh", AccountType: "Organization", OrgID: &orgID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Store.AcceptOrgAgreement(ctx, store.OrgAgreement{OrgID: orgID, VersionID: "2026-11", SHA256: "ab12", AcceptedBy: &e.alice.ID, AcceptedByRef: "user:alice", AcceptedAt: time.Now()}, []byte("the agreement")); err != nil {
		t.Fatal(err)
	}
	// The same file in globex/site: content-addressed, stored once.
	data := []byte("package shared\n")
	for _, c := range []struct {
		who  service.Actor
		repo string
	}{{e.alice, "acme/shop"}, {e.bob, "globex/site"}} {
		hs, err := e.svc.PutObjects(ctx, c.who, c.repo, []service.Object{{Kind: hash.KindBlob, Data: data}})
		if err != nil {
			t.Fatal(err)
		}
		shared = hs[0]
	}
	return shared
}

func TestPurgeOrgLeavesNothing(t *testing.T) {
	e := newAuthzEnv(t)
	shared := e.fillOrg()
	orgID := e.repo.OrgID
	ids, err := e.svc.Store.OrgRecordIDs(e.ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	own, sharedHashes, err := e.svc.Store.OrgObjectHashes(e.ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) < 3 || len(sharedHashes) == 0 {
		t.Fatalf("setup: %d objects of acme only, %d shared", len(own), len(sharedHashes))
	}
	// Before the purge, the walk finds acme everywhere: it does look.
	if left, err := e.svc.Store.PurgeLeftovers(e.ctx, ids, nil); err != nil || len(left) < 10 {
		t.Fatalf("leftovers before the purge: %v, %v", left, err)
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	// Deleting stops syncing at once: the installation is left alone, no
	// GitHub call is even tried.
	if err := e.svc.SyncInstallation(e.ctx, 77); err != nil {
		t.Errorf("syncing the installation of a deleted organization: %v", err)
	}
	if done, err := e.svc.PurgeDueOrgs(e.ctx, nil); err != nil || len(done) != 0 {
		t.Fatalf("purged during the grace period: %+v %v", done, err)
	}
	e.later(service.DefaultOrgDeleteGrace + time.Minute)
	done, err := e.svc.PurgeDueOrgs(e.ctx, nil)
	if err != nil || len(done) != 1 {
		t.Fatalf("PurgeDueOrgs = %+v, %v", done, err)
	}
	var objs [][]byte
	for _, h := range own {
		objs = append(objs, h.Bytes())
	}
	left, err := e.svc.Store.PurgeLeftovers(e.ctx, ids, objs)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("the purge left rows of acme in: %s", strings.Join(left, ", "))
	}
	for _, h := range own {
		if ok, _ := e.blobs.Has(e.ctx, h); ok {
			t.Errorf("object %s of acme is still in the blob store", h.Short())
		}
	}
	if ok, _ := e.blobs.Has(e.ctx, shared); !ok {
		t.Fatal("the purge deleted an object globex/site still uses")
	}
	if got, err := e.svc.GetObjects(e.ctx, e.bob, "globex/site", []hash.Hash{shared}, 0); err != nil || len(got) != 1 {
		t.Errorf("globex/site reads its shared object: %v", err)
	}
	// What is left: the deletion record and the lifecycle entries.
	recs, err := e.svc.ConsoleOrgDeletionRecords(e.ctx, "acme")
	if err != nil || len(recs) != 1 || recs[0].PurgedAt == nil {
		t.Fatalf("deletion record = %+v, %v", recs, err)
	}
	c := recs[0].Counts
	for k, want := range map[string]int64{"repositories": 1, "efforts": 1, "members": 2, "teams": 1, "github_installation_links": 1, "dpa_acceptances": 1} {
		if c[k] != want {
			t.Errorf("counts[%s] = %d, want %d (all: %v)", k, c[k], want, c)
		}
	}
	for _, k := range []string{"changes", "notes", "workspaces", "tasks", "claims", "reviews", "object_records", "blobs", "operations"} {
		if c[k] == 0 {
			t.Errorf("counts[%s] = 0 (all: %v)", k, c)
		}
	}
	if c["blobs"] != int64(len(own)) {
		t.Errorf("deleted %d blobs, acme alone had %d", c["blobs"], len(own))
	}
	text := recs[0].Confirmation("")
	if !strings.Contains(text, "Purged on") || strings.Contains(text, "shop") || strings.Contains(text, "Checkout") {
		t.Errorf("confirmation after the purge:\n%s", text)
	}
	if ops := e.ops("acme"); ops != "org.delete org.purge" {
		t.Errorf("operation log of acme after the purge: %q", ops)
	}
	// People stay; the handle is free again.
	if _, err := e.svc.Store.PrincipalByID(e.ctx, e.alice.ID); err != nil {
		t.Errorf("alice after the purge: %v", err)
	}
	if _, err := e.svc.CreateOrg(e.ctx, e.bob, "acme", ""); err != nil {
		t.Errorf("creating acme again after the purge: %v", err)
	}
	if recs, _ := e.svc.ConsoleOrgDeletionRecords(e.ctx, "acme"); len(recs) != 1 {
		t.Errorf("records after reusing the handle: %d", len(recs))
	}
}

// Two servers purge at once: one holds the lock, the other skips, and
// nothing is purged or counted twice. A purge that stops after any batch
// continues where it was.
func TestPurgeOneAtATimeAndResumable(t *testing.T) {
	e := newAuthzEnv(t)
	e.fillOrg()
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	e.later(service.DefaultOrgDeleteGrace + time.Minute)
	other := service.New(e.svc.Store, e.blobs, nil)
	other.Now = e.svc.Now

	saved := store.PurgeBatch
	store.PurgeBatch = 2
	t.Cleanup(func() { store.PurgeBatch = saved })

	// The first purger stops (as if its server died) after 5 batches.
	ctx, cancel := context.WithCancel(e.ctx)
	batches := 0
	service.SetPurgeHook(e.svc, func(string) {
		if batches++; batches == 5 {
			cancel()
		}
	})
	if _, err := e.svc.PurgeDueOrgs(ctx, nil); err == nil {
		t.Fatal("the stopped purge reported success")
	}
	// The second server takes over; while it works, a third is refused.
	inside := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	service.SetPurgeHook(other, func(string) {
		once.Do(func() { close(inside); <-release })
	})
	var wg sync.WaitGroup
	var done []service.OrgDeletion
	var perr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		done, perr = other.PurgeDueOrgs(e.ctx, nil)
	}()
	<-inside
	service.SetPurgeHook(e.svc, nil)
	if _, err := e.svc.PurgeDueOrgs(e.ctx, nil); !errors.Is(err, service.ErrPurgeBusy) {
		t.Errorf("a second purger at the same time: %v", err)
	}
	close(release)
	wg.Wait()
	if perr != nil || len(done) != 1 {
		t.Fatalf("resumed purge = %+v, %v", done, perr)
	}
	c := done[0].Counts
	if c["repositories"] != 1 || c["efforts"] != 1 || c["members"] != 2 {
		t.Errorf("counts after a resumed purge: %v", c)
	}
	if again, err := e.svc.PurgeDueOrgs(e.ctx, nil); err != nil || len(again) != 0 {
		t.Errorf("purging again: %+v, %v", again, err)
	}
}

// putHook calls hook after every Put, between the store having the object
// and the service recording it for a repository.
type putHook struct {
	blob.Store
	hook func(hash.Hash)
}

func (p *putHook) Put(ctx context.Context, kind hash.Kind, data []byte) (hash.Hash, error) {
	h, err := p.Store.Put(ctx, kind, data)
	if err == nil && p.hook != nil {
		p.hook(h)
	}
	return h, err
}

func (p *putHook) Unwrap() blob.Store { return p.Store }

// An upload of a file another, purged organization also had: the purge
// deletes the blob after the upload found it in the store (and skipped
// writing it) but before the upload recorded it. The upload notices and
// writes it again.
func TestPurgeRacingAnUpload(t *testing.T) {
	e := newAuthzEnv(t)
	data := []byte("the same file in two organizations\n")
	if _, err := e.svc.PutObjects(e.ctx, e.alice, "acme/shop", []service.Object{{Kind: hash.KindBlob, Data: data}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	e.later(service.DefaultOrgDeleteGrace + time.Minute)
	want := hash.Of(hash.KindBlob, data)
	hooked := &putHook{Store: e.blobs}
	e.svc.Blobs = hooked
	purged := false
	hooked.hook = func(h hash.Hash) {
		if h != want || purged {
			return
		}
		purged = true
		if done, err := e.svc.PurgeDueOrgs(e.ctx, nil); err != nil || len(done) != 1 {
			t.Errorf("purge during the upload: %+v, %v", done, err)
		}
		if ok, _ := e.blobs.Has(e.ctx, h); ok {
			t.Error("the purge kept the blob: the race did not happen")
		}
	}
	if _, err := e.svc.PutObjects(e.ctx, e.bob, "globex/site", []service.Object{{Kind: hash.KindBlob, Data: data}}); err != nil {
		t.Fatal(err)
	}
	if !purged {
		t.Fatal("the purge did not run during the upload")
	}
	if ok, _ := e.blobs.Has(e.ctx, want); !ok {
		t.Fatal("the uploaded blob is gone: the upload did not notice the purge")
	}
	got, err := e.svc.GetObjects(e.ctx, e.bob, "globex/site", []hash.Hash{want}, 0)
	if err != nil || len(got) != 1 || string(got[0].Data) != string(data) {
		t.Fatalf("GetObjects after the race = %v, %v", got, err)
	}
}

func TestConsolePurgeOrg(t *testing.T) {
	e := newAuthzEnv(t)
	if _, err := e.svc.ConsolePurgeOrg(e.ctx, "acme"); !isCode(err, service.CodeFailedPrecondition) {
		t.Errorf("purging an organization that is not deleted: %v", err)
	}
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	d, err := e.svc.ConsolePurgeOrg(e.ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if d.PurgedAt == nil || d.Counts["repositories"] != 1 {
		t.Errorf("console purge = %+v", d)
	}
	recs, err := e.svc.ConsoleOrgDeletionRecords(e.ctx, "acme")
	if err != nil || len(recs) != 1 || !strings.Contains(recs[0].Confirmation(""), "Purged on") {
		t.Fatalf("deletion record = %+v, %v", recs, err)
	}
	if _, err := e.svc.ConsolePurgeOrg(e.ctx, "acme"); !isCode(err, service.CodeNotFound) {
		t.Errorf("purging twice: %v", err)
	}
	if _, err := e.svc.ConsoleOrgDeletionRecords(e.ctx, "globex"); !isCode(err, service.CodeNotFound) {
		t.Errorf("record of an organization never deleted: %v", err)
	}
}

// fakeMirrors is an upstream with git mirrors on "disk".
type fakeMirrors struct {
	mu   sync.Mutex
	have map[uuid.UUID]bool
}

func (f *fakeMirrors) DefaultBranch(context.Context, store.Repo) (string, error) {
	return "", errors.New("no upstream in this test")
}

func (f *fakeMirrors) SyncBranch(context.Context, store.Repo, string) (store.Branch, int, error) {
	return store.Branch{}, 0, errors.New("no upstream in this test")
}

func (f *fakeMirrors) RemoveMirror(id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	had := f.have[id]
	delete(f.have, id)
	return had, nil
}

func (f *fakeMirrors) MirrorRepoIDs() ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for id := range f.have {
		out = append(out, id)
	}
	return out, nil
}

// The purging server removes the organization's git mirrors; every other
// server removes its own copies when it finds their repository gone.
func TestPurgeRemovesGitMirrors(t *testing.T) {
	e := newAuthzEnv(t)
	globex, err := e.svc.Store.RepoByName(e.ctx, "globex/site")
	if err != nil {
		t.Fatal(err)
	}
	here := &fakeMirrors{have: map[uuid.UUID]bool{e.repo.ID: true, globex.ID: true}}
	elsewhere := &fakeMirrors{have: map[uuid.UUID]bool{e.repo.ID: true, globex.ID: true}}
	e.svc.Upstream = here
	if _, err := e.svc.DeleteOrg(e.ctx, e.alice, "acme", "acme"); err != nil {
		t.Fatal(err)
	}
	other := service.New(e.svc.Store, e.blobs, elsewhere)
	if n, err := other.SweepMirrors(e.ctx); err != nil || n != 0 {
		t.Fatalf("sweeping during the grace period removed %d mirrors, %v", n, err)
	}
	d, err := e.svc.ConsolePurgeOrg(e.ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if d.Counts["mirrors"] != 1 || here.have[e.repo.ID] || !here.have[globex.ID] {
		t.Errorf("mirrors on the purging server: %v, counted %d", here.have, d.Counts["mirrors"])
	}
	if n, err := other.SweepMirrors(e.ctx); err != nil || n != 1 || elsewhere.have[e.repo.ID] || !elsewhere.have[globex.ID] {
		t.Errorf("sweep on another server = %d, %v; left %v", n, err, elsewhere.have)
	}
}
