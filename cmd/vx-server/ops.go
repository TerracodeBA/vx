package main

// Operations settings (#10, #38): /metrics, rate limits, quotas, audit
// retention; and the vx-server quota command.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TerracodeBA/vx/internal/ratelimit"
	"github.com/TerracodeBA/vx/internal/server"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// quotaEnv are the environment variables of the quota defaults, in
// service.QuotaNames order.
var quotaEnv = map[string]string{
	"repos":        "VX_QUOTA_REPOS",
	"stored":       "VX_QUOTA_STORED",
	"import":       "VX_QUOTA_IMPORT",
	"push":         "VX_QUOTA_PUSH",
	"open_efforts": "VX_QUOTA_OPEN_EFFORTS",
}

// applyOps fills cfg's operations settings from the environment.
func applyOps(cfg *server.Config, getenv func(string) string) error {
	cfg.MetricsToken = strings.TrimSpace(getenv("VX_METRICS_TOKEN"))
	limits, err := ratelimit.FromEnv(getenv)
	if err != nil {
		return err
	}
	cfg.Limits = limits
	q, err := quotaDefaults(getenv)
	if err != nil {
		return err
	}
	cfg.Quotas.Defaults = q
	if v := strings.TrimSpace(getenv("VX_DISK_WARN_PERCENT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 99 {
			return fmt.Errorf("VX_DISK_WARN_PERCENT=%q: give a whole number from 1 to 99", v)
		}
		cfg.Quotas.DiskWarnPercent = n
	}
	ret, err := service.ParseRetention(getenv("VX_AUDIT_RETENTION"))
	if err != nil {
		return fmt.Errorf("VX_AUDIT_RETENTION: %w", err)
	}
	cfg.Audit.Retention = ret
	return nil
}

func quotaDefaults(getenv func(string) string) (service.Quota, error) {
	var q service.Quota
	for _, name := range service.QuotaNames {
		key := quotaEnv[name]
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			continue
		}
		n, err := parseQuota(name, v)
		if err != nil {
			return q, fmt.Errorf("%s: %w", key, err)
		}
		switch name {
		case "repos":
			q.Repos = n
		case "stored":
			q.StoredBytes = n
		case "import":
			q.ImportBytes = n
		case "push":
			q.PushBytes = n
		case "open_efforts":
			q.OpenEfforts = n
		}
	}
	return q, nil
}

// parseQuota reads a count (repos, open_efforts) or a size (the others);
// 0 and "none" mean no limit.
func parseQuota(name, v string) (int64, error) {
	if name == "repos" || name == "open_efforts" {
		if v == "none" || v == "unlimited" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a count: give a whole number, 0 for no limit", v)
		}
		return n, nil
	}
	return service.ParseBytes(v)
}

func quotaCmd(dbURL *string) *cobra.Command {
	flags := map[string]*string{}
	cmd := &cobra.Command{
		Use:   "quota ORG",
		Short: "Show or change an organization's quotas",
		Long: `Show an organization's quotas and what it uses, or override the server's
defaults (VX_QUOTA_*) for it. Sizes take units (500MB, 10GB); 0 means no
limit; "default" goes back to the server's default.

  vx-server quota acme
  vx-server quota acme --repos 50 --stored 20GB --push default`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := openMigrated(ctx, *dbURL)
			if err != nil {
				return err
			}
			defer st.Close()
			var cfg server.Config
			if err := applyOps(&cfg, os.Getenv); err != nil {
				return err
			}
			svc := service.New(st, nil, nil)
			svc.Quotas = cfg.Quotas
			org, err := st.OrgByHandle(ctx, strings.ToLower(args[0]))
			if err != nil {
				return err
			}
			o, err := st.GetOrgQuota(ctx, org.ID)
			if err != nil {
				return err
			}
			changed := false
			for _, name := range service.QuotaNames {
				f := cmd.Flags().Lookup(flagName(name))
				if !f.Changed {
					continue
				}
				changed = true
				var p *int64
				if v := strings.TrimSpace(*flags[name]); v != "default" {
					n, err := parseQuota(name, v)
					if err != nil {
						return fmt.Errorf("--%s: %w", flagName(name), err)
					}
					p = &n
				}
				setQuotaField(&o, name, p)
			}
			var u service.QuotaUsage
			if changed {
				u, err = svc.ConsoleSetOrgQuota(ctx, org.Handle, o)
			} else {
				u, err = svc.ConsoleOrgQuota(ctx, org.Handle)
			}
			if err != nil {
				return err
			}
			if changed {
				fmt.Printf("Saved the quotas of %s.\n", org.Handle)
			}
			fmt.Printf("Quotas of %s (used / limit):\n", org.Handle)
			for _, r := range u.Rows() {
				src := ""
				if !r.Default {
					src = "  (set for this organization)"
				}
				used := r.Used
				if used == "" {
					used = "per request"
				}
				fmt.Printf("  %-14s %-18s %s%s\n", r.Label, used, r.Limit, src)
			}
			return nil
		},
	}
	for _, name := range service.QuotaNames {
		var v string
		flags[name] = &v
		cmd.Flags().StringVar(&v, flagName(name), "", quotaHelp[name])
	}
	return cmd
}

var quotaHelp = map[string]string{
	"repos":        "repositories (count; 0: no limit; default: the server's)",
	"stored":       "stored data over all repositories (like 20GB)",
	"import":       "size of one import from a git host (like 2GB)",
	"push":         "size of one upload of objects (like 500MB)",
	"open_efforts": "efforts open or shipping at once (count)",
}

func flagName(quota string) string { return strings.ReplaceAll(quota, "_", "-") }

func setQuotaField(o *store.OrgQuota, name string, v *int64) {
	switch name {
	case "repos":
		o.Repos = v
	case "stored":
		o.StoredBytes = v
	case "import":
		o.ImportBytes = v
	case "push":
		o.PushBytes = v
	case "open_efforts":
		o.OpenEfforts = v
	}
}
