package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/store"
)

// TestJob describes a job for EnqueueTestJob.
type TestJob struct {
	Check, Runner, Reporter string
	AllowSamePerson         bool
	Timeout                 time.Duration
	Author                  ChangeAuthor
	ChangeID                string
	Base, Tree              hash.Hash
	Values                  map[string]string
	Trigger                 string // default land
}

// EnqueueTestJob enqueues a job directly, for tests of the queue itself.
func EnqueueTestJob(ctx context.Context, s *Service, repo store.Repo, eff store.Effort, j TestJob) (store.CheckJob, bool, error) {
	if j.Trigger == "" {
		j.Trigger = "land"
	}
	return s.enqueueCheckJob(ctx, JobSpec{Repo: repo, Effort: eff, Trigger: j.Trigger,
		Def:    jobDef{Name: j.Check, Runner: j.Runner, Reporter: j.Reporter, AllowSamePerson: j.AllowSamePerson, Timeout: j.Timeout},
		Change: JobChangeRef{ID: j.ChangeID, Author: j.Author}, Base: j.Base, Tree: j.Tree, Values: j.Values})
}

// FinishTestJobs is what storing a result that counts does to the queue.
func FinishTestJobs(ctx context.Context, s *Service, repo store.Repo, effortID uuid.UUID, check string, tree hash.Hash) {
	s.afterCheckResult(ctx, repo, effortID, check, tree, uuid.New())
}
