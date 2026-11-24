package service_test

// Claims, tasks and rebases came from another effort, written before
// repository roles existed. They go through Authorize like everything else:
// no access looks like an unknown effort or id, read lets you look, write
// lets you change.

import (
	"testing"
	"time"

	"github.com/TerracodeBA/vx/internal/service"
)

func TestCoordinationHonoursRepositoryRoles(t *testing.T) {
	e := newAuthzEnv(t)
	ctx, slug := e.ctx, e.eff.Slug
	task, err := e.svc.AddTask(ctx, e.agent, service.AddTaskInput{Repo: "acme/shop", Effort: slug, Title: "Price rules"})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := e.svc.MakeClaim(ctx, e.agent, service.ClaimInput{Repo: "acme/shop", Effort: slug, Targets: []string{"cart.go"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	type op struct {
		name  string
		write bool
		run   func(service.Actor) error
	}
	ops := []op{
		{"ListClaims", false, func(a service.Actor) error {
			_, err := e.svc.ListClaims(ctx, a, "acme/shop", slug, nil)
			return err
		}},
		{"ListTasks", false, func(a service.Actor) error {
			_, err := e.svc.ListTasks(ctx, a, service.ListTasksInput{Repo: "acme/shop", Effort: slug})
			return err
		}},
		{"GetRebase", false, func(a service.Actor) error {
			_, _, err := e.svc.GetRebase(ctx, a, "acme/shop", slug)
			return err
		}},
		{"MakeClaim", true, func(a service.Actor) error {
			_, _, err := e.svc.MakeClaim(ctx, a, service.ClaimInput{Repo: "acme/shop", Effort: slug, Targets: []string{"x.go"}})
			return err
		}},
		{"AddTask", true, func(a service.Actor) error {
			_, err := e.svc.AddTask(ctx, a, service.AddTaskInput{Repo: "acme/shop", Effort: slug, Title: "Mine"})
			return err
		}},
		// By id, across efforts: the id alone must not reach into acme.
		{"UpdateTask by id", true, func(a service.Actor) error {
			_, _, err := e.svc.UpdateTask(ctx, a, service.UpdateTaskInput{ID: task.ID.String(), Action: "take"})
			return err
		}},
		{"RebaseEffort", true, func(a service.Actor) error {
			_, err := e.svc.RebaseEffort(ctx, a, "acme/shop", slug, "")
			return err
		}},
	}
	for _, who := range []service.Actor{e.bob, e.bobAgent} {
		e.grant(service.RoleNone)
		for _, o := range ops {
			if err := o.run(who); !isCode(err, service.CodeNotFound) {
				t.Errorf("%s by %s without access: %v, want not found", o.name, who.Ref(), err)
			}
		}
		e.grant(service.RoleRead)
		for _, o := range ops {
			err := o.run(who)
			if o.write && !isCode(err, service.CodePermission) {
				t.Errorf("%s by %s with read: %v, want a permission error", o.name, who.Ref(), err)
			}
			if !o.write && err != nil {
				t.Errorf("%s by %s with read: %v", o.name, who.Ref(), err)
			}
		}
	}
	// Releasing alice's claim by id: not found without access.
	e.grant(service.RoleNone)
	if _, err := e.svc.ReleaseClaims(ctx, e.bob, claim.ID.String(), ""); !isCode(err, service.CodeNotFound) {
		t.Errorf("release a claim in an unreadable repo: %v, want not found", err)
	}
	// With write, bob can claim, add and take tasks.
	e.grant(service.RoleWrite)
	if _, _, err := e.svc.MakeClaim(ctx, e.bob, service.ClaimInput{Repo: "acme/shop", Effort: slug, Targets: []string{"x.go"}}); err != nil {
		t.Errorf("claim with write: %v", err)
	}
	if _, _, err := e.svc.UpdateTask(ctx, e.bob, service.UpdateTaskInput{ID: task.ID.String(), Action: "take"}); err != nil {
		t.Errorf("take a task with write: %v", err)
	}
}
