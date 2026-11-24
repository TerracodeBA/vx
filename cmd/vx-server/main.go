// Command vx-server runs the vx server and its admin tasks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/gitbridge"
	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/version"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	// The first SIGINT or SIGTERM cancels ctx, with a cause naming the
	// signal, and starts a graceful shutdown. Stopping the notification then
	// lets a second signal end the process at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "vx-server:", err)
		os.Exit(1)
	}
}

// devDatabaseURL is the database of `make db`, for local development.
const devDatabaseURL = "postgres://vx:vx@127.0.0.1:54329/vx?sslmode=disable"

func root() *cobra.Command {
	var dbURL, dataDir string
	cmd := &cobra.Command{
		Use:           "vx-server",
		Short:         "The vx server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// The URL usually holds the database password, so --help must not show
	// the value from the environment: the flag's default is empty and the
	// environment is read when a command runs.
	cmd.PersistentFlags().StringVar(&dbURL, "db", "", "Postgres URL (default: VX_DATABASE_URL, else "+devDatabaseURL+")")
	cmd.PersistentPreRun = func(*cobra.Command, []string) {
		if dbURL == "" {
			dbURL = env("VX_DATABASE_URL", devDatabaseURL)
		}
	}
	cmd.PersistentFlags().StringVar(&dataDir, "data", env("VX_DATA_DIR", ".vxdata"), "data directory for objects and git mirrors (VX_DATA_DIR)")

	var addr string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			log.Info("starting vx-server", "version", version.String())
			gh, err := githubSettings()
			if err != nil {
				return err
			}
			srv, err := server.New(cmd.Context(), server.Config{
				Addr: addr, DatabaseURL: dbURL, DataDir: dataDir, Logger: log, Upstream: upstream, GitHub: gh,
			})
			if err != nil {
				return err
			}
			defer srv.Close()
			return srv.ListenAndServe(cmd.Context(), addr)
		},
	}
	serve.Flags().StringVar(&addr, "addr", env("VX_ADDR", "127.0.0.1:7070"), "listen address (VX_ADDR)")

	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := store.Open(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.Migrate(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("Database is up to date.")
			return nil
		},
	}

	var name, email, userOrg string
	var siteAdmin bool
	userAdd := &cobra.Command{
		Use:   "user-add HANDLE",
		Short: "Create a person, their personal organization and a first token",
		Long: `Create a person and print their first token. Every person gets a personal
organization named like them. --admin makes them a site admin (may manage
people and every organization). --org adds them to an organization as a
member, or creates it with them as owner when it does not exist yet.

The first person on a new server: vx-server user-add alice --admin --org acme`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openMigrated(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			svc := service.New(st, nil, nil)
			p, tok, createdOrg, err := svc.AddPerson(cmd.Context(), nil, service.NewPerson{
				Handle: args[0], DisplayName: name, Email: email, SiteAdmin: siteAdmin, Org: userOrg, CreateOrg: true,
			})
			if err != nil {
				return err
			}
			fmt.Printf("Created %s with personal organization %s.\n", p.Ref(), p.Handle)
			if p.SiteAdmin {
				fmt.Println("They are a site admin.")
			}
			switch {
			case createdOrg:
				fmt.Printf("Created organization %s with them as owner.\n", userOrg)
			case userOrg != "":
				fmt.Printf("Added them to organization %s as a member.\n", userOrg)
			}
			fmt.Printf("Token (shown once): %s\n\n  vx login --server http://%s --token %s\n", tok, env("VX_ADDR", "127.0.0.1:7070"), tok)
			return nil
		},
	}
	userAdd.Flags().StringVar(&name, "name", "", "display name, used as git author name")
	userAdd.Flags().StringVar(&email, "email", "", "email, used as git author email")
	userAdd.Flags().BoolVar(&siteAdmin, "admin", false, "make them a site admin")
	userAdd.Flags().StringVar(&userOrg, "org", "", "add them to this organization (created with them as owner if missing)")

	var inviteNote string
	var inviteAdmin bool
	invite := &cobra.Command{
		Use:   "invite GITHUB_LOGIN",
		Short: "Let a GitHub user create an account by signing in with GitHub",
		Long: `Invite a GitHub login while sign-up is invite-only (the default). The person
then opens the server and chooses Sign in with GitHub; the invite is used up
on their first sign-in. Site admins can also invite on Admin > People.

--admin makes them a site admin when they sign up: use it for the first admin
of a new server.

  vx-server invite octocat --note "from the waitlist"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openMigrated(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			inv, err := service.New(st, nil, nil).InviteGitHubLogin(cmd.Context(), nil, args[0], inviteNote, inviteAdmin)
			if err != nil {
				return err
			}
			fmt.Printf("Invited GitHub user %s.", inv.GitHubLogin)
			if inv.SiteAdmin {
				fmt.Print(" They become a site admin when they sign up.")
			}
			fmt.Println("\nThey open the server in a browser and choose Sign in with GitHub.")
			return nil
		},
	}
	invite.Flags().StringVar(&inviteNote, "note", "", "why they are invited, shown to site admins")
	invite.Flags().BoolVar(&inviteAdmin, "admin", false, "make them a site admin when they sign up")

	tokenCreate := &cobra.Command{
		Use:   "token-create HANDLE",
		Short: "Issue another token for a person or agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openMigrated(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			p, err := st.PrincipalByHandle(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			tok, err := st.CreateToken(cmd.Context(), p.ID, "created by vx-server token-create")
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
	var backfillRepo string
	backfill := &cobra.Command{
		Use:   "backfill-objects",
		Short: "Record which objects belong to which repository, for data from before v0.2",
		Long: `Objects are served only through repositories they are part of. Repositories
created before v0.2 have their objects indexed once: the server does it on
start, in the background, and on first use of each repository. This command
does the same by hand: every queued repository, or one with --repo (walked
again even when already indexed). It is safe to run while the server runs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			st, err := openMigrated(ctx, dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			blobs, err := blob.NewDisk(filepath.Join(dataDir, "objects"))
			if err != nil {
				return err
			}
			svc := service.New(st, blobs, nil)
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			var done []service.BackfillStats
			switch {
			case backfillRepo != "":
				repo, err := st.RepoByName(ctx, backfillRepo)
				if err != nil {
					return fmt.Errorf("repo %s: %w", backfillRepo, err)
				}
				s, err := svc.BackfillRepoObjects(ctx, repo.ID, true, log)
				if err != nil {
					return err
				}
				done = append(done, s)
			default:
				if done, err = svc.BackfillObjects(ctx, log); err != nil {
					return err
				}
			}
			if len(done) == 0 {
				fmt.Println("No repository needed an object backfill.")
				return nil
			}
			for _, s := range done {
				fmt.Printf("%s: %d changesets, %d objects recorded", s.Repo, s.Changesets, s.Objects)
				if s.Skipped > 0 {
					fmt.Printf(", %d trees missing from the object store", s.Skipped)
				}
				fmt.Println()
			}
			return nil
		},
	}
	backfill.Flags().StringVar(&backfillRepo, "repo", "", "only this repository (OWNER/NAME), even if already indexed")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the vx-server version",
		Run:   func(*cobra.Command, []string) { fmt.Println("vx-server " + version.String()) },
	}
	cmd.AddCommand(serve, migrate, userAdd, invite, tokenCreate, backfill, versionCmd)
	return cmd
}

func openMigrated(ctx context.Context, url string) (*store.Store, error) {
	st, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// upstream builds the git bridge. GitHub access uses VX_GITHUB_TOKEN.
func upstream(st *store.Store, blobs blob.Store, dataDir string) (service.UpstreamSyncer, error) {
	token := func() string { return os.Getenv("VX_GITHUB_TOKEN") }
	b := gitbridge.New(st, blobs, filepath.Join(dataDir, "git"), token)
	// GitHub Enterprise Server (checked by githubSettings).
	if v := strings.TrimRight(os.Getenv("VX_GITHUB_URL"), "/"); v != "" {
		b.GitHubGitBase = v
	}
	if v := strings.TrimRight(os.Getenv("VX_GITHUB_API_URL"), "/"); v != "" {
		b.GitHubAPI = v
	}
	return b, nil
}
