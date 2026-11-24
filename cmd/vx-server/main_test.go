package main

import (
	"bytes"
	"strings"
	"testing"
)

// The database URL carries the password; --help must never print the value
// it got from the environment.
func TestHelpHidesDatabaseURL(t *testing.T) {
	t.Setenv("VX_DATABASE_URL", "postgres://vx:s3cret-password@db:5432/vx")
	for _, args := range [][]string{{"--help"}, {"user-add", "--help"}, {"serve", "--help"}} {
		cmd := root()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out.String(), "s3cret-password") {
			t.Errorf("vx-server %s shows the password:\n%s", strings.Join(args, " "), out.String())
		}
		if args[0] != "--help" && !strings.Contains(out.String(), "VX_DATABASE_URL") {
			t.Errorf("vx-server %s no longer says where the URL comes from:\n%s", strings.Join(args, " "), out.String())
		}
	}
}

func TestDatabaseURLFromEnvironment(t *testing.T) {
	t.Setenv("VX_DATABASE_URL", "postgres://from-env")
	cmd := root()
	cmd.SetArgs([]string{"serve", "--help"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	// PersistentPreRun does not run for --help; run it as a command would.
	cmd.PersistentPreRun(cmd, nil)
	if got := cmd.PersistentFlags().Lookup("db").Value.String(); got != "postgres://from-env" {
		t.Errorf("db = %q, want the environment's URL", got)
	}
}
