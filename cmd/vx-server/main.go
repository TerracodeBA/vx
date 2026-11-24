// Command vx-server runs the vx server and its admin tasks.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "vx-server:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	var dbURL, dataDir string
	cmd := &cobra.Command{
		Use:           "vx-server",
		Short:         "The vx server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&dbURL, "db", env("VX_DATABASE_URL", "postgres://vx:vx@127.0.0.1:54329/vx?sslmode=disable"), "Postgres URL (VX_DATABASE_URL)")
	cmd.PersistentFlags().StringVar(&dataDir, "data", env("VX_DATA_DIR", ".vxdata"), "data directory for objects and git mirrors (VX_DATA_DIR)")

	var addr string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			srv, err := server.New(cmd.Context(), server.Config{
				Addr: addr, DatabaseURL: dbURL, DataDir: dataDir, Logger: log, Upstream: upstream,
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

	var name, email string
	userAdd := &cobra.Command{
		Use:   "user-add HANDLE",
		Short: "Create a person and print their first token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openMigrated(cmd.Context(), dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			p, err := st.CreatePrincipal(cmd.Context(), store.Principal{Kind: store.KindHuman, Handle: args[0], DisplayName: name, Email: email})
			if err != nil {
				return err
			}
			tok, err := st.CreateToken(cmd.Context(), p.ID, "created by vx-server user-add")
			if err != nil {
				return err
			}
			fmt.Printf("Created %s.\nToken (shown once): %s\n\n  vx login --server http://%s --token %s\n", p.Ref(), tok, env("VX_ADDR", "127.0.0.1:7070"), tok)
			return nil
		},
	}
	userAdd.Flags().StringVar(&name, "name", "", "display name, used as git author name")
	userAdd.Flags().StringVar(&email, "email", "", "email, used as git author email")

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
	cmd.AddCommand(serve, migrate, userAdd, tokenCreate)
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

// upstream builds the git bridge. It is filled in once the bridge exists.
var upstream func(st *store.Store, blobs blob.Store, dataDir string) (service.UpstreamSyncer, error)

