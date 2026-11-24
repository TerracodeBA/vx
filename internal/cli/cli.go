// Package cli implements the vx command line. Every command prints plain
// text for people and, with --json, structured output for agents.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/version"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitConflict = 2 // land or sync stopped on conflicts
)

// App holds what commands need from the environment, so tests can run the
// CLI in-process.
type App struct {
	Out    io.Writer
	Err    io.Writer
	Getenv func(string) string
	Dir    string // working directory

	jsonOut  bool
	chdir    string
	exitCode int
}

// errConflict makes Run exit with ExitConflict after the command printed
// the conflicts.
var errConflict = errors.New("stopped on conflicts")

// Run executes the CLI and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	root := a.rootCmd()
	root.SetArgs(args)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	err := root.ExecuteContext(ctx)
	switch {
	case err == nil:
		return a.exitCode
	case errors.Is(err, errConflict):
		return ExitConflict
	}
	if a.jsonOut {
		_ = json.NewEncoder(a.Out).Encode(map[string]string{"error": client.Message(err)})
	} else {
		fmt.Fprintln(a.Err, "vx:", client.Message(err))
	}
	return ExitError
}

func (a *App) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vx",
		Short:         "vx: a workbench where agents build together before code goes to git",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `vx lets many agents, from many people, work on one effort at the same time
and ship the result to GitHub or any git host as clean pull requests.

Start here:
  vx login --server URL --token TOKEN
  vx repo create acme/shop-api --github acme/shop-api
  vx effort new "Rate limiting" --repo acme/shop-api
  vx clone acme/shop-api rate-limiting
  cd rate-limiting && vx new "Add limiter middleware"
  ...edit files...
  vx land`,
	}
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "print JSON instead of text")
	root.PersistentFlags().StringVarP(&a.chdir, "dir", "C", "", "run as if started in this directory")
	root.AddCommand(
		a.loginCmd(), a.whoamiCmd(), a.agentCmd(),
		a.repoCmd(), a.effortCmd(),
		a.cloneCmd(), a.newCmd(), a.statusCmd(), a.diffCmd(), a.landCmd(), a.syncCmd(),
		a.abandonCmd(), a.logCmd(), a.showCmd(), a.shipCmd(), a.waitCmd(),
		&cobra.Command{
			Use:   "version",
			Short: "Print the vx version",
			Run: func(cmd *cobra.Command, _ []string) {
				a.printf("vx %s\n", version.String())
				a.emit(map[string]string{"version": version.Version, "build": version.String()})
			},
		},
	)
	return root
}

func (a *App) dir() string {
	if a.chdir != "" {
		if filepath.IsAbs(a.chdir) {
			return a.chdir
		}
		return filepath.Join(a.Dir, a.chdir)
	}
	return a.Dir
}

// ---------- configuration ----------

// Settings is the user's vx configuration file.
type Settings struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func (a *App) settingsPath() string {
	if p := a.Getenv("VX_CONFIG"); p != "" {
		return p
	}
	if x := a.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "vx", "config.json")
	}
	home := a.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "vx", "config.json")
}

func (a *App) settings() Settings {
	var s Settings
	if data, err := os.ReadFile(a.settingsPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if v := a.Getenv("VX_SERVER"); v != "" {
		s.Server = v
	}
	if v := a.Getenv("VX_TOKEN"); v != "" {
		s.Token = v
	}
	return s
}

func (a *App) client() (*client.Client, error) {
	s := a.settings()
	if s.Server == "" || s.Token == "" {
		return nil, errors.New("not logged in: run `vx login --server URL --token TOKEN` or set VX_SERVER and VX_TOKEN")
	}
	return client.New(s.Server, s.Token), nil
}

func (a *App) workingCopy() (*workcopy.WorkingCopy, error) {
	return workcopy.Find(a.dir())
}

// ---------- output ----------

func (a *App) printf(format string, args ...any) {
	if !a.jsonOut {
		fmt.Fprintf(a.Out, format, args...)
	}
}

var pj = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}

// emit prints v as JSON when --json is set.
func (a *App) emit(v any) {
	if !a.jsonOut {
		return
	}
	if m, ok := v.(proto.Message); ok {
		data, err := pj.Marshal(m)
		if err == nil {
			fmt.Fprintln(a.Out, string(data))
			return
		}
	}
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func short(h string) string {
	h = strings.TrimPrefix(h, "b3:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func ctxOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
