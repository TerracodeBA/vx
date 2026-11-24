package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/version"
)

// ---------- vx login ----------

func (a *App) loginCmd() *cobra.Command {
	var server, token, name string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to a vx server: in your browser, or with a token",
		Long: `Log in to a vx server and save the server and your token in vx's config.

Without --token, vx opens the server's approval page in your browser and
shows a code. Sign in there (with GitHub, for example), check that the page
shows the same code, and click Approve; vx then saves a new token named
"vx on HOST (browser login)". The code works once, for ten minutes.
--no-browser only prints the link, for example on a remote machine: open it
on any device where you can sign in.

With --token (or VX_TOKEN set), vx checks that token and saves it instead.

Agents never log in through a browser: a person creates an agent with
` + "`vx agent add NAME --env-file FILE`" + ` and the agent uses that file's token.`,
		Example: `  vx login --server https://vx.example.com
  vx login --server https://vx.example.com --no-browser
  vx login --server http://localhost:7070 --token vxt_...`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctxOf(cmd)
			if server == "" {
				st := a.settings()
				if st.err != nil && a.profile == "" {
					return st.err
				}
				server = st.Server
			}
			if server == "" {
				return errors.New("which server? Run vx login --server URL, e.g. vx login --server https://vx.example.com")
			}
			server = normalizeServer(server)
			profileName, err := a.loginProfileName(server)
			if err != nil {
				return err
			}
			fromEnv := false
			if token == "" && a.Getenv("VX_TOKEN") != "" {
				token, fromEnv = a.Getenv("VX_TOKEN"), true
			}
			info, infoErr := a.fetchServerInfo(ctx, server)
			if infoErr == nil {
				if info.CLIStatus == "update_required" {
					return errors.New(info.Message)
				}
				a.notice(info)
			}
			var principal *vxv1.Principal
			if token != "" {
				resp, err := client.New(server, token).Auth.WhoAmI(ctx, connect.NewRequest(&vxv1.WhoAmIRequest{}))
				if err != nil {
					if client.Code(err) == connect.CodeUnauthenticated {
						return fmt.Errorf("%s does not accept this token (%s); check it, or run vx login --server %s without --token to log in in your browser", server, client.Message(err), server)
					}
					return err
				}
				principal = resp.Msg.Principal
			} else {
				if infoErr != nil {
					if serverLacksPublic(infoErr) {
						return fmt.Errorf("%s runs an older vx-server without browser login; log in with a token instead: vx login --server %s --token TOKEN (ask whoever runs the server for one)", server, server)
					}
					return fmt.Errorf("could not reach %s: %s", server, client.Message(infoErr))
				}
				if name == "" {
					name = defaultClientName()
				}
				var err error
				if token, principal, err = a.browserLogin(ctx, server, name, noBrowser); err != nil {
					return err
				}
			}
			if infoErr != nil {
				info = nil
			}
			if err := a.saveLogin(profileName, Profile{Server: server, Token: token, Principal: principal.Ref}, info); err != nil {
				return err
			}
			a.printf("Logged in to %s as %s (profile %s, now current)\n", server, principal.Ref, profileName)
			if fromEnv {
				a.printf("(with the token from VX_TOKEN)\n")
			} else if a.Getenv("VX_TOKEN") != "" {
				fmt.Fprintln(a.Err, "vx: notice: VX_TOKEN is set and takes precedence over the saved token; unset it to use this login")
			}
			a.emit(map[string]any{"server": server, "principal": principal.Ref, "on_behalf_of": principal.OnBehalfOf, "profile": profileName, "config": a.settingsPath()})
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "server URL, e.g. https://vx.example.com (default: the saved server)")
	cmd.Flags().StringVar(&token, "token", "", "log in with this token instead of the browser")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the approval link instead of opening a browser")
	cmd.Flags().StringVar(&name, "name", "", `what the token is called, e.g. "vx on work-laptop" (default: vx on HOSTNAME)`)
	return cmd
}

// normalizeServer adds https:// to a bare host and drops trailing slashes.
func normalizeServer(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if !strings.Contains(s, "://") {
		if strings.HasPrefix(s, "localhost") || strings.HasPrefix(s, "127.0.0.1") {
			return "http://" + s
		}
		return "https://" + s
	}
	return s
}

func defaultClientName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "vx CLI"
	}
	host, _, _ = strings.Cut(host, ".")
	return "vx on " + host
}

// browserLogin runs the device-code flow: start, show the code and link,
// open the browser, poll until approved, denied or expired.
func (a *App) browserLogin(ctx context.Context, server, name string, noBrowser bool) (string, *vxv1.Principal, error) {
	c := client.New(server, "")
	start, err := c.Public.StartCLILogin(ctx, connect.NewRequest(&vxv1.StartCLILoginRequest{ClientName: name}))
	if err != nil {
		return "", nil, err
	}
	l := start.Msg
	expires, _ := time.Parse(time.RFC3339, l.ExpiresAt)
	if expires.IsZero() {
		expires = time.Now().Add(10 * time.Minute)
	}
	// With --json the instructions go to stderr, so stdout stays one JSON
	// document.
	say := func(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }
	if a.jsonOut {
		say = func(format string, args ...any) { fmt.Fprintf(a.Err, format, args...) }
	}
	say("Your login code: %s\n", l.UserCode)
	say("Approve it in your browser, after checking the page shows the same code:\n  %s\n", l.VerificationUrl)
	if !noBrowser {
		open := a.OpenBrowser
		if open == nil {
			open = openBrowser
		}
		if err := open(l.VerificationUrl); err != nil {
			say("Could not open a browser (%s); open the link above yourself.\n", err)
		} else {
			say("Opened it in your browser.\n")
		}
	}
	say("Waiting for approval (the code works for %d minutes; Ctrl-C cancels)...\n", max(1, int(time.Until(expires).Round(time.Minute).Minutes())))

	interval := time.Duration(l.IntervalSeconds) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	failures := 0
	for wait := time.Duration(0); ; {
		select {
		case <-ctx.Done():
			return "", nil, errors.New("login cancelled; nothing was saved. Run vx login again when you are ready")
		case <-time.After(wait):
		}
		wait = interval
		if time.Now().After(expires.Add(5 * time.Second)) {
			return "", nil, errors.New("the code expired before it was approved; run vx login again for a new one")
		}
		resp, err := c.Public.PollCLILogin(ctx, connect.NewRequest(&vxv1.PollCLILoginRequest{DeviceCode: l.DeviceCode}))
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, errors.New("login cancelled; nothing was saved. Run vx login again when you are ready")
			}
			if client.Code(err) == connect.CodeUnavailable || client.Code(err) == connect.CodeDeadlineExceeded || client.Code(err) == 0 {
				if failures++; failures < 5 {
					continue // a network hiccup: keep polling
				}
			}
			return "", nil, fmt.Errorf("waiting for approval failed: %s", client.Message(err))
		}
		failures = 0
		switch resp.Msg.Status {
		case "pending":
		case "slow_down":
			interval += time.Second
		case "approved":
			if resp.Msg.Token == "" || resp.Msg.Principal == nil {
				return "", nil, errors.New("the server approved the login but sent no token; run vx login again")
			}
			return resp.Msg.Token, resp.Msg.Principal, nil
		case "denied":
			return "", nil, errors.New("the login was denied in the browser; nothing was saved. Run vx login again if that was a mistake")
		case "expired":
			return "", nil, errors.New("the code expired before it was approved (codes work for ten minutes); run vx login again for a new one")
		case "used":
			return "", nil, errors.New("this login was already collected by another vx; run vx login again")
		default:
			return "", nil, fmt.Errorf("the server answered %q; run vx login again", resp.Msg.Status)
		}
	}
}

// ---------- config file ----------

// serverInfo is what vx remembers of a server's ServerInfo answer.
type serverInfo struct {
	Server       string    `json:"server"`
	CheckedAt    time.Time `json:"checked_at"`
	Version      string    `json:"version"`
	MinCLI       string    `json:"min_cli_version"`
	Features     []string  `json:"features"`
	GitHubSignIn bool      `json:"github_sign_in"`
	CLIVersion   string    `json:"cli_version"` // the vx that asked
	CLIStatus    string    `json:"cli_status"`
	Message      string    `json:"message,omitempty"`
	// OlderAt is when vx last said it is older than the server.
	OlderAt time.Time `json:"older_noticed_at,omitempty"`
}

// olderNoticeEvery is how often vx repeats that it is older than the
// server: once per command would drown the output, once a day is missed.
const olderNoticeEvery = time.Hour

// serverInfoTTL is how often vx asks the server for its version.
const serverInfoTTL = 24 * time.Hour

// fileSettings is the selected profile of the config file, without
// VX_SERVER and VX_TOKEN.
func (a *App) fileSettings() Settings {
	cf := a.readConfig()
	name, source := a.profileChoice(cf)
	p, ok := cf.Profiles[name]
	if !ok {
		s := Settings{}
		if source != "" {
			s.err = a.errNoProfile(name, cf)
		}
		return s
	}
	return Settings{Server: p.Server, Token: p.Token, ServerInfo: cf.Servers[strings.TrimRight(p.Server, "/")], Profile: name}
}

// saveLogin saves a login as profile name (made current) and what the
// server said about itself.
func (a *App) saveLogin(name string, p Profile, info *serverInfo) error {
	return a.updateConfig(func(cf *configFile) error {
		cf.Profiles[name] = &p
		cf.Current = name
		if info != nil {
			cf.Servers[info.Server] = info
		}
		return nil
	})
}

// loginProfileName is the profile a login to server goes to: --profile,
// else a saved profile for the same server, else the server's host.
func (a *App) loginProfileName(server string) (string, error) {
	if a.profile != "" {
		return a.profile, validProfileName(a.profile)
	}
	cf := a.readConfig()
	if cur, ok := cf.Profiles[cf.Current]; ok && strings.TrimRight(cur.Server, "/") == server {
		return cf.Current, nil
	}
	var names []string
	for n, p := range cf.Profiles {
		if strings.TrimRight(p.Server, "/") == server {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		slices.Sort(names)
		return names[0], nil
	}
	name := hostOf(server)
	if validProfileName(name) != nil {
		return "default", nil
	}
	return name, nil
}

// fetchServerInfo asks server for its version and features.
func (a *App) fetchServerInfo(ctx context.Context, server string) (*serverInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.New(server, "").Public.ServerInfo(ctx, connect.NewRequest(&vxv1.ServerInfoRequest{CliVersion: version.Version}))
	if err != nil {
		return nil, err
	}
	m := resp.Msg
	return &serverInfo{
		Server: server, CheckedAt: time.Now(), Version: m.Version, MinCLI: m.MinCliVersion, Features: m.Features,
		GitHubSignIn: m.GithubSignIn, CLIVersion: version.Version, CLIStatus: m.CliStatus, Message: m.Message,
	}, nil
}

// serverLacksPublic reports whether err says the server has no
// PublicService: a vx-server from before browser login.
func serverLacksPublic(err error) bool {
	switch client.Code(err) {
	case connect.CodeUnimplemented, connect.CodeNotFound:
		return true
	case connect.CodeUnknown:
		return strings.Contains(client.Message(err), "404")
	}
	return false
}

// notice prints one line when vx should be updated: that this vx is older
// than the server's release (vx self-update fixes it), or else what the
// server recommends.
func (a *App) notice(info *serverInfo) {
	if info == nil {
		return
	}
	if msg := version.OlderThanServer(version.Version, info.Version); msg != "" {
		fmt.Fprintln(a.Err, "vx:", msg)
		info.OlderAt = time.Now()
		return
	}
	if info.CLIStatus != "" && info.CLIStatus != "ok" && info.Message != "" {
		fmt.Fprintln(a.Err, "vx: notice:", info.Message)
	}
}

// checkServer asks the server for its version at most once a day per
// server and CLI version, remembers the answer in the config file and
// prints a notice when vx should be updated. It never fails a command:
// when the server cannot answer, the next command asks again.
func (a *App) checkServer(server string) {
	if a.checked || server == "" {
		return
	}
	a.checked = true
	server = strings.TrimRight(server, "/")
	if c := a.readConfig().Servers[server]; c != nil && c.CLIVersion == version.Version && time.Since(c.CheckedAt) < serverInfoTTL {
		// Known: repeat "older than the server" now and then, without asking.
		if msg := version.OlderThanServer(version.Version, c.Version); msg != "" && time.Since(c.OlderAt) >= olderNoticeEvery {
			fmt.Fprintln(a.Err, "vx:", msg)
			_ = a.updateConfig(func(cf *configFile) error {
				if cur := cf.Servers[server]; cur != nil {
					cur.OlderAt = time.Now()
				}
				return nil
			})
		}
		return
	}
	info, err := a.fetchServerInfo(context.Background(), server)
	if err != nil {
		if !serverLacksPublic(err) {
			return
		}
		// An older server: remember that, so commands do not ask each time.
		info = &serverInfo{Server: server, CheckedAt: time.Now(), CLIVersion: version.Version}
	}
	a.notice(info)
	_ = a.updateConfig(func(cf *configFile) error {
		cf.Servers[server] = info
		return nil
	})
}

// knownServerInfo is the remembered ServerInfo of the current server.
func (a *App) knownServerInfo() *serverInfo {
	return a.readConfig().Servers[strings.TrimRight(a.settings().Server, "/")]
}

// featureErr turns "this server does not know this RPC" into one line that
// names the missing feature and what to do, using what the server said
// about itself.
func (a *App) featureErr(feature, what string, err error) error {
	server := strings.TrimRight(a.settings().Server, "/")
	if info := a.knownServerInfo(); info != nil && info.Version != "" {
		if !slices.Contains(info.Features, feature) {
			return fmt.Errorf("this vx server (%s, vx-server %s) has no %s; ask whoever runs it to upgrade vx-server", server, info.Version, what)
		}
		return fmt.Errorf("this vx server (%s, vx-server %s) did not answer for %s (%s); ask whoever runs it to check or upgrade vx-server", server, info.Version, what, client.Message(err))
	}
	return fmt.Errorf("this vx server (%s) runs an older vx-server without %s (%s); ask whoever runs it to upgrade vx-server", server, what, client.Message(err))
}
