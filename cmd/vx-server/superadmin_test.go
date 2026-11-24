package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// TestSuperadminCommand: grant, revoke and list from the console, with op
// log entries and clear refusals for agents and unknown handles.
func TestSuperadminCommand(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	run := func(args ...string) (string, error) {
		cmd := root()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--db", dbURL}, args...))
		err := cmd.ExecuteContext(ctx)
		return out.String(), err
	}
	if out, err := run("superadmin", "list"); err != nil || !strings.Contains(out, "No superadmins") {
		t.Fatalf("list on an empty server = %q %v", out, err)
	}
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindAgent, Handle: "alice/claude", ParentID: &alice.ID}); err != nil {
		t.Fatal(err)
	}
	if out, err := run("superadmin", "grant", "alice"); err != nil || out != "user:alice is now a superadmin.\n" {
		t.Fatalf("grant = %q %v", out, err)
	}
	if out, err := run("superadmin", "grant", "user:alice"); err != nil || !strings.Contains(out, "already is a superadmin") {
		t.Fatalf("grant again = %q %v", out, err)
	}
	if out, err := run("superadmin", "list"); err != nil || out != "user:alice\tAlice\talice@example.com\n" {
		t.Fatalf("list = %q %v", out, err)
	}
	if _, err := run("superadmin", "grant", "alice/claude"); err == nil || !strings.Contains(err.Error(), "only people can be superadmins") {
		t.Fatalf("grant to an agent: %v", err)
	}
	if _, err := run("superadmin", "grant", "nobody"); err == nil || !strings.Contains(err.Error(), "there is no person nobody") {
		t.Fatalf("grant to nobody: %v", err)
	}
	if out, err := run("superadmin", "revoke", "alice"); err != nil || !strings.Contains(out, "user:alice is no longer a superadmin.") || !strings.Contains(out, "Nobody is a superadmin now") {
		t.Fatalf("revoke = %q %v", out, err)
	}
	if p, _ := st.PrincipalByHandle(ctx, "alice"); p.SiteAdmin {
		t.Fatal("still a superadmin after revoke")
	}
	ops := opsOf(t, st, "user:alice")
	if strings.Join(ops, ",") != "person.superadmin_grant,person.superadmin_revoke" {
		t.Fatalf("op log = %v", ops)
	}
}

// opsOf lists the op log entries for target, oldest first.
func opsOf(t *testing.T, st *store.Store, target string) []string {
	t.Helper()
	entries, err := st.ListTargetOps(context.Background(), target, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.ActorID != nil {
			t.Errorf("%s has actor %v; console ops have none", e.Op, e.ActorID)
		}
		out = append(out, e.Op)
	}
	return out
}
