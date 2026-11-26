package service_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// queueEnv is authzEnv with a clock, runner agents and two trees of
// acme/shop's effort.
type queueEnv struct {
	*authzEnv
	now          time.Time
	sibling      service.Actor // alice/browser: another agent of the author's person
	author       service.ChangeAuthor
	base, t1, t2 hash.Hash
}

func newQueueEnv(t *testing.T) *queueEnv {
	t.Helper()
	q := &queueEnv{authzEnv: newAuthzEnv(t), now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	q.svc.Now = func() time.Time { return q.now }
	q.grant(service.RoleWrite)
	sib, err := q.svc.Store.CreatePrincipal(q.ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/browser", ParentID: &q.alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	q.sibling, _ = q.svc.ResolveActor(q.ctx, sib)
	for _, set := range []struct {
		who   service.Actor
		agent string
	}{{q.alice, "browser"}, {q.bob, "claude"}} {
		if _, err := q.svc.SetAgentRunner(q.ctx, set.who, set.agent, []string{"browser"}); err != nil {
			t.Fatal(err)
		}
	}
	q.author = service.ChangeAuthor{Principal: q.agent.Ref(), Person: q.alice.Ref()}
	eff, _ := q.svc.Store.EffortByID(q.ctx, q.eff.ID)
	head, _ := q.svc.Store.GetChangeset(q.ctx, q.repo.ID, eff.Head)
	q.base = head.Tree
	q.t1 = q.snapshot(q.repo, eff.Head, map[string]*string{"web/a.html": s("<p>one</p>\n")})
	q.t2 = q.snapshot(q.repo, eff.Head, map[string]*string{"web/a.html": s("<p>two</p>\n")})
	return q
}

func (q *queueEnv) enqueue(j service.TestJob) store.CheckJob {
	q.t.Helper()
	if j.Runner == "" {
		j.Runner = "agent:browser"
	}
	if j.Author.Principal == "" {
		j.Author = q.author
	}
	if j.Tree.IsZero() {
		j.Base, j.Tree = q.base, q.t1
	}
	eff, _ := q.svc.Store.EffortByID(q.ctx, q.eff.ID)
	out, _, err := service.EnqueueTestJob(q.ctx, q.svc, q.repo, eff, j)
	if err != nil {
		q.t.Fatal(err)
	}
	return out
}

func (q *queueEnv) next(a service.Actor, runner string) *service.CheckJobView {
	q.t.Helper()
	v, err := q.svc.NextCheckJob(q.ctx, a, service.NextJobInput{Repo: "acme/shop", Runner: runner})
	if err != nil {
		q.t.Fatalf("next for %s: %v", a.Ref(), err)
	}
	return v
}

func (q *queueEnv) job(id fmt.Stringer) store.CheckJob {
	q.t.Helper()
	vs, err := q.svc.ListCheckJobs(q.ctx, q.alice, service.ListJobsInput{Repo: "acme/shop"})
	if err != nil {
		q.t.Fatal(err)
	}
	for _, v := range vs {
		if v.ID.String() == id.String() {
			return v.CheckJob
		}
	}
	q.t.Fatalf("job %s not listed", id)
	return store.CheckJob{}
}

// TestCheckJobLeases: take, renew, expire with the attempt counted, fail
// after the retry limit, release without counting.
func TestCheckJobLeases(t *testing.T) {
	q := newQueueEnv(t)
	j := q.enqueue(service.TestJob{Check: "e2e", Reporter: "independent", Timeout: 10 * time.Minute, ChangeID: "aaaa-bbbb",
		Values: map[string]string{"preview_url": "https://pr-1.example.com"}})

	// The author's sibling agent may not report an independent check.
	if v := q.next(q.sibling, ""); v != nil {
		t.Fatalf("the author's sibling got %s", v.Check)
	}
	v := q.next(q.bobAgent, "browser")
	if v == nil || v.ID != j.ID || v.State != store.JobLeased || v.LeaseOwnerRef != q.bobAgent.Ref() {
		t.Fatalf("bob's agent took %+v", v)
	}
	if !v.LeaseExpiresAt.Equal(q.now.Add(10 * time.Minute)) {
		t.Fatalf("lease until %v", v.LeaseExpiresAt)
	}
	in := v.Inputs
	if in == nil || in.Values["preview_url"] != "https://pr-1.example.com" || len(in.ChangedFiles) != 1 || in.ChangedFiles[0].Path != "web/a.html" ||
		len(in.Files) != 1 || in.Files[0].Hash == "" || !strings.Contains(in.Diff, "+<p>one</p>") || in.Effort.Slug != q.eff.Slug || in.Change.ID != "aaaa-bbbb" {
		t.Fatalf("inputs = %+v", in)
	}
	if again := q.next(q.bobAgent, ""); again != nil {
		t.Fatalf("a leased job was handed out again: %s", again.ID)
	}

	if r, err := q.svc.RenewCheckJob(q.ctx, q.bobAgent, "acme/shop", j.ID.String()[:8], 20*time.Minute); err != nil || !r.LeaseExpiresAt.Equal(q.now.Add(20*time.Minute)) {
		t.Fatalf("renew: %+v %v", r.LeaseExpiresAt, err)
	}
	if _, err := q.svc.RenewCheckJob(q.ctx, q.sibling, "acme/shop", j.ID.String(), 0); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "leased to agent:bob/claude") {
		t.Fatalf("renew by someone else: %v", err)
	}
	if _, err := q.svc.RenewCheckJob(q.ctx, q.bobAgent, "acme/shop", j.ID.String(), 25*time.Hour); !isCode(err, service.CodeInvalid) {
		t.Fatalf("renew for 25h: %v", err)
	}

	// Expiry puts it back with the attempt counted; the third fails it.
	for attempt := 1; attempt <= service.MaxJobAttempts; attempt++ {
		if attempt > 1 {
			if v := q.next(q.bobAgent, ""); v == nil || v.ID != j.ID {
				t.Fatalf("attempt %d: no job", attempt)
			}
		}
		q.now = q.now.Add(time.Hour)
		got := q.job(j.ID)
		if attempt < service.MaxJobAttempts {
			if got.State != store.JobQueued || got.Attempts != attempt || !strings.Contains(got.Reason, "expired without a result") || got.LeaseOwner != nil {
				t.Fatalf("after expiry %d: %+v", attempt, got)
			}
			if _, err := q.svc.RenewCheckJob(q.ctx, q.bobAgent, "acme/shop", j.ID.String(), 0); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "no lease") {
				t.Fatalf("renewing an expired lease: %v", err)
			}
		} else if got.State != store.JobFailed || !strings.Contains(got.Reason, "expired 3 times") || !strings.Contains(got.Reason, "bob/claude") {
			t.Fatalf("after the last expiry: %+v", got)
		}
	}
	if v := q.next(q.bobAgent, ""); v != nil {
		t.Fatalf("a failed job was handed out: %+v", v)
	}

	// Release: back in the queue, no attempt counted, the reason kept.
	r := q.enqueue(service.TestJob{Check: "e2e-mobile", Reporter: "self"})
	if v := q.next(q.bobAgent, ""); v == nil || v.ID != r.ID {
		t.Fatal("no job to release")
	}
	rel, err := q.svc.ReleaseCheckJob(q.ctx, q.bobAgent, "acme/shop", r.ID.String(), "no phone emulator here")
	if err != nil || rel.State != store.JobQueued || rel.Attempts != 0 || !strings.Contains(rel.Reason, "no phone emulator here") {
		t.Fatalf("release: %+v %v", rel, err)
	}
	if _, err := q.svc.ReleaseCheckJob(q.ctx, q.bobAgent, "acme/shop", r.ID.String(), ""); !isCode(err, service.CodeFailedPrecondition) {
		t.Fatalf("releasing twice: %v", err)
	}
	// A self check may go to the author's own sibling.
	if v := q.next(q.sibling, ""); v == nil || v.ID != r.ID {
		t.Fatalf("self check for the sibling: %+v", v)
	}
}

// TestCheckJobRunnersAndTrust: jobs go only to runners of their kind, and
// never to a runner whose result would not count.
func TestCheckJobRunnersAndTrust(t *testing.T) {
	q := newQueueEnv(t)
	sec := q.enqueue(service.TestJob{Check: "security", Runner: "agent:security", Reporter: "independent"})
	if v := q.next(q.bobAgent, ""); v != nil {
		t.Fatalf("a browser runner got %s", v.Check)
	}
	if _, err := q.svc.NextCheckJob(q.ctx, q.bobAgent, service.NextJobInput{Repo: "acme/shop", Runner: "security"}); !isCode(err, service.CodeInvalid) {
		t.Fatalf("asking for a kind it does not serve: %v", err)
	}
	if _, err := q.svc.SetAgentRunner(q.ctx, q.bob, "claude", []string{"browser", "security"}); err != nil {
		t.Fatal(err)
	}
	if v := q.next(q.bobAgent, "agent:security"); v == nil || v.ID != sec.ID {
		t.Fatalf("security runner: %+v", v)
	}

	// allow_same_person: the author's sibling may run it, the author not.
	same := q.enqueue(service.TestJob{Check: "visual", Reporter: "independent", AllowSamePerson: true})
	if _, err := q.svc.SetAgentRunner(q.ctx, q.alice, "claude", []string{"browser"}); err != nil {
		t.Fatal(err)
	}
	if v := q.next(q.agent, ""); v != nil {
		t.Fatalf("the author took its own independent check %s", v.Check)
	}
	if v := q.next(q.sibling, ""); v == nil || v.ID != same.ID {
		t.Fatalf("allow_same_person for the sibling: %+v", v)
	}

	// Human jobs: people, never agents; on For you until taken.
	hum := q.enqueue(service.TestJob{Check: "signoff", Runner: "human", Reporter: "human"})
	if hs, err := q.svc.HumanChecksFor(q.ctx, q.bob); err != nil || len(hs) != 1 || hs[0].JobID != hum.ID || hs[0].Effort != q.eff.Slug {
		t.Fatalf("bob's human checks: %+v %v", hs, err)
	}
	if hs, _ := q.svc.HumanChecksFor(q.ctx, q.alice); len(hs) != 1 {
		t.Fatalf("alice signs off her agent's change: %+v", hs)
	}
	if hs, _ := q.svc.HumanChecksFor(q.ctx, q.bobAgent); len(hs) != 0 {
		t.Fatalf("an agent sees human checks: %+v", hs)
	}
	if v := q.next(q.bobAgent, ""); v != nil {
		t.Fatalf("an agent took %s", v.Check)
	}
	if v := q.next(q.bob, "human"); v == nil || v.ID != hum.ID {
		t.Fatalf("bob takes the sign-off: %+v", v)
	}
	if hs, _ := q.svc.HumanChecksFor(q.ctx, q.alice); len(hs) != 0 {
		t.Fatalf("a sign-off bob holds is still on alice's page: %+v", hs)
	}
	// A change alice made by hand: not hers to sign off.
	byHand := q.enqueue(service.TestJob{Check: "signoff2", Runner: "human", Reporter: "human", Author: service.ChangeAuthor{Principal: "user:alice", Person: "user:alice"}})
	if v := q.next(q.alice, ""); v != nil {
		t.Fatalf("alice took the sign-off of her own change: %s", v.Check)
	}
	if v := q.next(q.bob, ""); v == nil || v.ID != byHand.ID {
		t.Fatalf("bob signs off alice's change: %+v", v)
	}

	// CI jobs: only CI tokens of the repository, for the checks they may report.
	unit := q.enqueue(service.TestJob{Check: "unit", Runner: "ci", Reporter: "ci"})
	q.enqueue(service.TestJob{Check: "lint", Runner: "ci", Reporter: "ci"})
	_, secret, err := q.svc.CreateCIToken(q.ctx, q.alice, "acme/shop", "gha", []string{"unit"})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := q.ciActor(secret)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := q.svc.NextCheckJob(q.ctx, ci, service.NextJobInput{}); err != nil || v == nil || v.ID != unit.ID {
		t.Fatalf("CI token takes unit: %+v %v", v, err)
	}
	if v, err := q.svc.NextCheckJob(q.ctx, ci, service.NextJobInput{}); err != nil || v != nil {
		t.Fatalf("CI token took a check it may not report: %+v %v", v, err)
	}
	if v := q.next(q.bob, ""); v != nil {
		t.Fatalf("a person took a ci job: %s", v.Check)
	}
	if _, err := q.svc.GetCheckJob(q.ctx, ci, "", unit.ID.String()); err != nil {
		t.Fatalf("CI token reads its job: %v", err)
	}
}

// TestCheckJobsSupersede: a new tree supersedes pending jobs of the same
// check; land and ship never make a second job for a tree; a result
// finishes the job; a request runs it again.
func TestCheckJobsSupersede(t *testing.T) {
	q := newQueueEnv(t)
	j1 := q.enqueue(service.TestJob{Check: "e2e", Tree: q.t1, Base: q.base})
	if v := q.next(q.bobAgent, ""); v == nil || v.ID != j1.ID {
		t.Fatal("no job")
	}
	j2 := q.enqueue(service.TestJob{Check: "e2e", Tree: q.t2, Base: q.base})
	old := q.job(j1.ID)
	if old.State != store.JobSuperseded || old.SupersededBy == nil || *old.SupersededBy != j2.ID || !strings.Contains(old.Reason, "newer tree") {
		t.Fatalf("old job: %+v", old)
	}
	if _, err := q.svc.RenewCheckJob(q.ctx, q.bobAgent, "acme/shop", j1.ID.String(), 0); !isCode(err, service.CodeFailedPrecondition) || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("renewing a superseded job: %v", err)
	}
	// Another check on the same effort is not affected.
	other := q.enqueue(service.TestJob{Check: "a11y", Tree: q.t1, Base: q.base})
	if q.job(j2.ID).State != store.JobQueued || q.job(other.ID).State != store.JobQueued {
		t.Fatal("a job of another check was superseded")
	}
	// The same tree again: the same job, values merged by a request.
	again := q.enqueue(service.TestJob{Check: "e2e", Tree: q.t2, Base: q.base})
	req := q.enqueue(service.TestJob{Check: "e2e", Tree: q.t2, Base: q.base, Trigger: "manual", Values: map[string]string{"preview_url": "https://x"}})
	if again.ID != j2.ID || req.ID != j2.ID || req.Values["preview_url"] != "https://x" {
		t.Fatalf("same tree: %s %s, want %s; values %v", again.ID, req.ID, j2.ID, req.Values)
	}

	service.FinishTestJobs(q.ctx, q.svc, q.repo, q.eff.ID, "e2e", q.t2)
	if got := q.job(j2.ID); got.State != store.JobDone || got.ResultID == "" {
		t.Fatalf("after a result: %+v", got)
	}
	if land := q.enqueue(service.TestJob{Check: "e2e", Tree: q.t2, Base: q.base}); land.ID != j2.ID {
		t.Fatal("land made a second job for a tree that has a result")
	}
	if rerun := q.enqueue(service.TestJob{Check: "e2e", Tree: q.t2, Base: q.base, Trigger: "manual"}); rerun.ID == j2.ID || rerun.State != store.JobQueued {
		t.Fatalf("a request did not run it again: %+v", rerun)
	}
}

// TestCheckJobsTwoServers: two servers sharing one database hand out every
// job exactly once to runners taking at the same time.
func TestCheckJobsTwoServers(t *testing.T) {
	q := newQueueEnv(t)
	other := service.New(q.svc.Store, q.svc.Blobs, nil)
	other.Now = q.svc.Now
	const n = 24
	for i := range n {
		q.enqueue(service.TestJob{Check: fmt.Sprintf("e2e-%02d", i), Reporter: "independent"})
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for w := range 8 {
		svc := q.svc
		if w%2 == 1 {
			svc = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				v, err := svc.NextCheckJob(q.ctx, q.bobAgent, service.NextJobInput{Repo: "acme/shop"})
				if err != nil {
					t.Error(err)
					return
				}
				if v == nil {
					return
				}
				mu.Lock()
				got[v.ID.String()]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != n {
		t.Fatalf("%d distinct jobs handed out, want %d", len(got), n)
	}
	for id, c := range got {
		if c != 1 {
			t.Errorf("job %s handed out %d times", id, c)
		}
	}
}
