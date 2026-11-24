package cli

// Profiles: one saved login per vx server, in one config file.
//
// The config file holds named profiles (server, token, what the server
// said about itself) and which one is current. vx login --server URL adds
// or refreshes a profile, named after the server's host unless --profile
// names it, and makes it current; vx use NAME switches; vx --profile NAME
// or VX_PROFILE picks one for a single command.
//
// VX_SERVER and VX_TOKEN (agent env files) still win over any profile, and
// VX_CONFIG still picks the file. A config file from before profiles (one
// server and token at the top level) becomes a profile named after its
// host the first time vx reads it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Profile is one saved login.
type Profile struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// Principal is who the token belonged to at login, for vx profiles.
	Principal string `json:"principal,omitempty"`
}

// configFile is the config file on disk.
type configFile struct {
	Current  string              `json:"current,omitempty"`
	Profiles map[string]*Profile `json:"profiles,omitempty"`
	// Servers is what each server said about itself (ServerInfo), by
	// server URL, asked at most once a day; also for VX_SERVER logins.
	Servers map[string]*serverInfo `json:"servers,omitempty"`

	// The single-server format from before profiles; read, never written.
	Server     string      `json:"server,omitempty"`
	Token      string      `json:"token,omitempty"`
	ServerInfo *serverInfo `json:"server_info,omitempty"`
}

// profileNameRe is what profile names look like: a host, host:port, or a
// short word.
var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)

func validProfileName(name string) error {
	if !profileNameRe.MatchString(name) {
		return fmt.Errorf("profile name %q: use letters, digits, dot, dash, underscore or colon, like app.vxbench.com or work", name)
	}
	return nil
}

// hostOf is the profile name for a server URL: its host (with the port,
// when there is one).
func hostOf(server string) string {
	u, err := url.Parse(normalizeServer(server))
	if err != nil || u.Host == "" {
		return strings.TrimRight(server, "/")
	}
	return u.Host
}

// readConfig reads the config file. A file in the old single-server format
// is migrated: its server becomes a profile named after the host, made
// current, and the file is rewritten (when that fails, the next read
// migrates again).
func (a *App) readConfig() configFile {
	var cf configFile
	if data, err := os.ReadFile(a.settingsPath()); err == nil {
		_ = json.Unmarshal(data, &cf)
	}
	if cf.Profiles == nil {
		cf.Profiles = map[string]*Profile{}
	}
	if cf.Servers == nil {
		cf.Servers = map[string]*serverInfo{}
	}
	if cf.Server != "" || cf.Token != "" || cf.ServerInfo != nil {
		if len(cf.Profiles) == 0 && cf.Server != "" {
			name := hostOf(cf.Server)
			cf.Profiles[name] = &Profile{Server: strings.TrimRight(cf.Server, "/"), Token: cf.Token}
			if cf.Current == "" {
				cf.Current = name
			}
		}
		if i := cf.ServerInfo; i != nil && i.Server != "" && cf.Servers[i.Server] == nil {
			cf.Servers[i.Server] = i
		}
		cf.Server, cf.Token, cf.ServerInfo = "", "", nil
		_ = a.writeConfig(cf)
	}
	return cf
}

// writeConfig writes the config file (mode 600) through a rename, so a
// reader never sees half a file.
func (a *App) writeConfig(cf configFile) error {
	cf.Server, cf.Token, cf.ServerInfo = "", "", nil
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.settingsPath(), append(data, '\n'))
}

// updateConfig reads the config file, lets fn change it and writes it back.
func (a *App) updateConfig(fn func(*configFile) error) error {
	cf := a.readConfig()
	if err := fn(&cf); err != nil {
		return err
	}
	return a.writeConfig(cf)
}

// profileChoice is the profile this run uses and why: the --profile flag,
// VX_PROFILE, or the config file's current profile.
func (a *App) profileChoice(cf configFile) (name, source string) {
	switch {
	case a.profile != "":
		return a.profile, "--profile"
	case a.Getenv("VX_PROFILE") != "":
		return a.Getenv("VX_PROFILE"), "VX_PROFILE"
	}
	return cf.Current, ""
}

// errNoProfile is the error for a profile that is not in the config file.
func (a *App) errNoProfile(name string, cf configFile) error {
	names := make([]string, 0, len(cf.Profiles))
	for n := range cf.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	have := "there are none yet"
	if len(names) > 0 {
		have = "saved: " + strings.Join(names, ", ")
	}
	return fmt.Errorf("no profile %q (%s); add it with vx login --server URL --profile %s, or list them with vx profiles", name, have, name)
}

// ---------- vx use, vx profiles ----------

// findProfile finds a profile by name, or by server URL or host.
func findProfile(cf configFile, arg string) (string, bool) {
	if _, ok := cf.Profiles[arg]; ok {
		return arg, true
	}
	want := normalizeServer(arg)
	host := hostOf(arg)
	var hits []string
	for n, p := range cf.Profiles {
		if strings.TrimRight(p.Server, "/") == want || hostOf(p.Server) == host {
			hits = append(hits, n)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return "", false
}

func (a *App) useCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use NAME",
		Short: "Switch to another saved server login (profile)",
		Long: `Make a saved profile the current one: later vx commands talk to its server
with its token. NAME is a profile name from vx profiles, or its server's URL
or host. vx login --server URL adds profiles; vx --profile NAME runs one
command with another profile.

VX_SERVER and VX_TOKEN (for example from an agent's env file) still take
precedence over the current profile.`,
		Example: `  vx use app.vxbench.com
  vx use localhost:7070`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			var prof Profile
			err := a.updateConfig(func(cf *configFile) error {
				n, ok := findProfile(*cf, args[0])
				if !ok {
					return a.errNoProfile(args[0], *cf)
				}
				name, prof = n, *cf.Profiles[n]
				cf.Current = n
				return nil
			})
			if err != nil {
				return err
			}
			a.printf("Now using %s (%s", name, prof.Server)
			if prof.Principal != "" {
				a.printf(", as %s", prof.Principal)
			}
			a.printf(")\n")
			a.envNotice()
			a.emit(map[string]any{"current": name, "server": prof.Server, "principal": prof.Principal})
			return nil
		},
	}
}

// envNotice says when VX_SERVER or VX_TOKEN override the saved profiles.
func (a *App) envNotice() {
	if a.Getenv("VX_TOKEN") != "" || a.Getenv("VX_SERVER") != "" {
		fmt.Fprintln(a.Err, "vx: notice: VX_SERVER or VX_TOKEN is set and takes precedence over saved profiles; unset them to use the profile")
	}
}

func (a *App) profilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "List saved server logins (profiles) and which one is current",
		Long: `List the profiles in vx's config file: one saved login per server. The
current one is marked with *. vx login --server URL adds one, vx use NAME
switches, vx profiles remove NAME forgets one (its token keeps working on
the server until you revoke it there).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cf := a.readConfig()
			active, source := a.profileChoice(cf)
			names := make([]string, 0, len(cf.Profiles))
			for n := range cf.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			type row struct {
				Name      string `json:"name"`
				Server    string `json:"server"`
				Principal string `json:"principal,omitempty"`
				Current   bool   `json:"current"`
				Active    bool   `json:"active"`
			}
			rows := []row{}
			for _, n := range names {
				p := cf.Profiles[n]
				rows = append(rows, row{Name: n, Server: p.Server, Principal: p.Principal, Current: n == cf.Current, Active: n == active})
			}
			env := a.Getenv("VX_TOKEN") != "" || a.Getenv("VX_SERVER") != ""
			if len(rows) == 0 {
				a.printf("No profiles yet. Add one: vx login --server URL\n")
			}
			for _, r := range rows {
				mark := " "
				if r.Active {
					mark = "*"
				}
				a.printf("%s %-24s %s", mark, r.Name, r.Server)
				if r.Principal != "" {
					a.printf("  %s", r.Principal)
				}
				a.printf("\n")
			}
			if source != "" && active != "" {
				a.printf("(%s picks %s for this command; the current profile is %s)\n", source, active, orNone(cf.Current))
			}
			if env {
				a.printf("VX_SERVER or VX_TOKEN is set and takes precedence over these profiles.\n")
			}
			a.emit(map[string]any{"current": cf.Current, "active": active, "env_overrides": env, "profiles": rows, "config": a.settingsPath()})
			return nil
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "remove NAME",
		Short: "Forget a saved profile (the token stays valid on the server until revoked)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name, next string
			err := a.updateConfig(func(cf *configFile) error {
				n, ok := findProfile(*cf, args[0])
				if !ok {
					return a.errNoProfile(args[0], *cf)
				}
				name = n
				delete(cf.Profiles, n)
				if cf.Current == n {
					cf.Current = ""
					if len(cf.Profiles) == 1 {
						for other := range cf.Profiles {
							cf.Current = other
						}
					}
				}
				next = cf.Current
				return nil
			})
			if err != nil {
				return err
			}
			a.printf("Removed profile %s. Its token still works on the server; revoke it there with vx token revoke if you no longer need it.\n", name)
			if next != "" {
				a.printf("Current profile: %s\n", next)
			}
			a.emit(map[string]any{"removed": name, "current": next})
			return nil
		},
	})
	return cmd
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// writeFileAtomic writes path (mode 600) through a rename in the same
// directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var errProfileAndToken = errors.New("--profile and VX_TOKEN are both set, and vx cannot tell which login you mean: unset VX_TOKEN (and VX_SERVER) to use the profile, or drop --profile to use the token")
