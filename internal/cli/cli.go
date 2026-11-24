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
	ExitTimeout  = 4 // vx wait gave up: nothing happened within --timeout
)

// App holds what commands need from the environment, so tests can run the
// CLI in-process.
type App struct {
	Out    io.Writer
	Err    io.Writer
	Getenv func(string) string
	Dir    string // working directory
	// OpenBrowser opens a URL for vx login and vx ui; nil uses the
	// system's browser. Tests set it to act as the browser.
	OpenBrowser func(url string) error
	// Executable is the path of the running vx, for vx self-update; nil
	// uses os.Executable. Tests point it at a file of their own.
	Executable func() (string, error)

	jsonOut  bool
	checked  bool // the server's version was checked in this run
	chdir    string
	profile  string // --profile
	exitCode int
	// landResult is the one line vx land and vx sync end with: what
	// landed or synced, or why nothing did. landTail are the lines right
	// before it: the conflicts to resolve.
	landResult string
	landTail   []string
	printed    bool // printf wrote something in this run
	// noGenerate (--no-generate) stops vx from running [[generated]]
	// commands after a merge kept generated files from one side.
	noGenerate bool
}

// exitError is an error that makes Run exit with code instead of
// ExitError, after printing it like any other error.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

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
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
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
  vx login --server URL
  vx repo create acme/shop-api --github acme/shop-api
  vx effort new "Rate limiting" --repo acme/shop-api
  vx clone acme/shop-api rate-limiting
  cd rate-limiting && vx new "Add limiter middleware"
  ...edit files...
  vx land`,
	}
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "print JSON instead of text")
	root.PersistentFlags().StringVarP(&a.chdir, "dir", "C", "", "run as if started in this directory")
	root.PersistentFlags().StringVar(&a.profile, "profile", "", "use this saved login (see vx profiles; default: VX_PROFILE, else the current one)")
	root.AddCommand(
		a.loginCmd(), a.useCmd(), a.profilesCmd(), a.whoamiCmd(), a.uiCmd(), a.agentCmd(), a.tokenCmd(),
		a.repoCmd(), a.orgCmd(), a.teamCmd(), a.effortCmd(),
		a.cloneCmd(), a.newCmd(), a.statusCmd(), a.diffCmd(), a.landCmd(), a.syncCmd(),
		a.abandonCmd(), a.logCmd(), a.showCmd(), a.shipCmd(), a.waitCmd(), a.mcpCmd(),
		a.postCmd(), a.askCmd(), a.answerCmd(), a.boardCmd(), a.inboxCmd(), a.contextCmd(),
		a.claimCmd(), a.releaseCmd(), a.claimsCmd(), a.overlapsCmd(),
		a.taskCmd(), a.tasksCmd(), a.rulesCmd(), a.reserveCmd(), a.reviewCmd(), a.selfUpdateCmd(),
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

// Settings is the login a command uses: the selected profile of the
// config file (profiles.go), overridden by VX_SERVER and VX_TOKEN.
type Settings struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// ServerInfo is the server's last ServerInfo answer, asked at most
	// once a day.
	ServerInfo *serverInfo `json:"server_info,omitempty"`
	// Profile is the name of the profile in use ("" without one).
	Profile string `json:"-"`
	// err is why no usable login was found (an unknown --profile).
	err error
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
	s := a.fileSettings()
	server, token := a.Getenv("VX_SERVER"), a.Getenv("VX_TOKEN")
	if token != "" && a.profile != "" {
		s.err = errProfileAndToken
	}
	if server != "" || token != "" {
		if s.err != nil && token == "" {
			s.err = nil // VX_SERVER alone: the profile's token on that server
		}
		if server != "" {
			s.Server = server
		}
		if token != "" {
			s.Token = token
		}
	}
	return s
}

func (a *App) client() (*client.Client, error) {
	s := a.settings()
	if s.err != nil {
		return nil, s.err
	}
	if s.Server == "" || s.Token == "" {
		return nil, errors.New("not logged in: run `vx login --server URL` (it opens your browser), or set VX_SERVER and VX_TOKEN")
	}
	a.checkServer(s.Server)
	return client.New(s.Server, s.Token), nil
}

func (a *App) workingCopy() (*workcopy.WorkingCopy, error) {
	return workcopy.Find(a.dir())
}

// ---------- output ----------

func (a *App) printf(format string, args ...any) {
	if !a.jsonOut {
		fmt.Fprintf(a.Out, format, args...)
		a.printed = true
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
