package service

// Quotas per organization (#38): repositories, stored bytes, the size of
// one import or upload, and open efforts. The server's defaults come from
// its configuration (VX_QUOTA_*); superadmins override them per
// organization. A refusal says what is used and what the limit is.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/metrics"
	"github.com/TerracodeBA/vx/internal/store"
)

// Quota is one set of limits; 0 means no limit.
type Quota struct {
	Repos       int64 // repositories
	StoredBytes int64 // bytes in the object store over all repositories
	ImportBytes int64 // size of one repository imported from a git host
	PushBytes   int64 // bytes of one upload of objects (one request)
	OpenEfforts int64 // efforts that are open or shipping
}

// QuotaSettings configure quotas and the disk-space warning.
type QuotaSettings struct {
	Defaults Quota
	// DataDir is the server's data directory, whose disk the admin panel
	// watches. Empty: no disk-space warning.
	DataDir string
	// DiskWarnPercent warns on the admin pages when less than this share
	// of the data disk is free. 0: 10.
	DiskWarnPercent int
}

// QuotaNames are the quotas in the order pages and the CLI show them.
var QuotaNames = []string{"repos", "stored", "import", "push", "open_efforts"}

// get returns the limit called name.
func (q Quota) get(name string) int64 {
	switch name {
	case "repos":
		return q.Repos
	case "stored":
		return q.StoredBytes
	case "import":
		return q.ImportBytes
	case "push":
		return q.PushBytes
	case "open_efforts":
		return q.OpenEfforts
	}
	return 0
}

// Effective is the defaults with an organization's override applied.
func (q Quota) Effective(o store.OrgQuota) Quota {
	pick := func(def int64, v *int64) int64 {
		if v != nil {
			return *v
		}
		return def
	}
	return Quota{
		Repos:       pick(q.Repos, o.Repos),
		StoredBytes: pick(q.StoredBytes, o.StoredBytes),
		ImportBytes: pick(q.ImportBytes, o.ImportBytes),
		PushBytes:   pick(q.PushBytes, o.PushBytes),
		OpenEfforts: pick(q.OpenEfforts, o.OpenEfforts),
	}
}

// QuotaUsage is what an organization uses against its limits.
type QuotaUsage struct {
	Org         string
	Limits      Quota          // in effect
	Defaults    Quota          // the server's
	Override    store.OrgQuota // the organization's own; nil fields: default
	Repos       int64
	OpenEfforts int64
	// StoredBytes is the bytes the organization's objects take in the
	// object store; StoredKnown is false while the server does not count
	// them yet.
	StoredBytes int64
	StoredKnown bool
}

// Row is one line of the quota table on the org page.
type QuotaRow struct {
	Name, Label string
	Used        string // "" when the quota is per request (import, push)
	Limit       string // "no limit" for 0
	Default     bool   // the limit is the server's default
	Over        bool   // used is at or above the limit
}

// Rows lists the quotas for display.
func (u QuotaUsage) Rows() []QuotaRow {
	labels := map[string]string{"repos": "Repositories", "stored": "Stored data", "import": "One import", "push": "One upload", "open_efforts": "Open efforts"}
	over := map[string]*int64{"repos": u.Override.Repos, "stored": u.Override.StoredBytes, "import": u.Override.ImportBytes, "push": u.Override.PushBytes, "open_efforts": u.Override.OpenEfforts}
	var out []QuotaRow
	for _, n := range QuotaNames {
		lim := u.Limits.get(n)
		row := QuotaRow{Name: n, Label: labels[n], Limit: "no limit", Default: over[n] == nil}
		bytes := n == "stored" || n == "import" || n == "push"
		if lim > 0 {
			row.Limit = strconv.FormatInt(lim, 10)
			if bytes {
				row.Limit = FormatBytes(lim)
			}
		}
		switch n {
		case "repos":
			row.Used, row.Over = strconv.FormatInt(u.Repos, 10), lim > 0 && u.Repos >= lim
		case "open_efforts":
			row.Used, row.Over = strconv.FormatInt(u.OpenEfforts, 10), lim > 0 && u.OpenEfforts >= lim
		case "stored":
			row.Used = "not measured yet"
			if u.StoredKnown {
				row.Used, row.Over = FormatBytes(u.StoredBytes), lim > 0 && u.StoredBytes >= lim
			}
		}
		out = append(out, row)
	}
	return out
}

// orgStoredBytes asks the store for the bytes an organization's objects
// take, when the store counts them (object sizes, from object storage).
func (s *Service) orgStoredBytes(ctx context.Context, orgID uuid.UUID) (int64, bool, error) {
	c, ok := any(s.Store).(interface {
		OrgStoredBytes(ctx context.Context, orgID uuid.UUID) (int64, error)
	})
	if !ok {
		return 0, false, nil
	}
	n, err := c.OrgStoredBytes(ctx, orgID)
	return n, err == nil, err
}

// usage reads an organization's usage and limits.
func (s *Service) usage(ctx context.Context, org store.Org) (QuotaUsage, error) {
	o, err := s.Store.GetOrgQuota(ctx, org.ID)
	if err != nil {
		return QuotaUsage{}, err
	}
	u := QuotaUsage{Org: org.Handle, Defaults: s.Quotas.Defaults, Override: o, Limits: s.Quotas.Defaults.Effective(o)}
	if u.Repos, u.OpenEfforts, err = s.Store.OrgUsage(ctx, org.ID); err != nil {
		return QuotaUsage{}, err
	}
	if u.StoredBytes, u.StoredKnown, err = s.orgStoredBytes(ctx, org.ID); err != nil {
		return QuotaUsage{}, err
	}
	return u, nil
}

// OrgQuota returns an organization's quotas and usage, for its members
// and superadmins.
func (s *Service) OrgQuota(ctx context.Context, actor Actor, org string) (QuotaUsage, error) {
	a, err := s.loadOrg(ctx, nil, actor, org)
	if err != nil {
		return QuotaUsage{}, err
	}
	u, err := s.usage(ctx, a.org)
	return u, classify(err)
}

// ConsoleOrgQuota is OrgQuota from the server console (vx-server quota).
func (s *Service) ConsoleOrgQuota(ctx context.Context, org string) (QuotaUsage, error) {
	o, err := s.Store.OrgByHandle(ctx, strings.ToLower(strings.TrimSpace(org)))
	if err != nil {
		return QuotaUsage{}, classify(err)
	}
	u, err := s.usage(ctx, o)
	return u, classify(err)
}

// SetOrgQuota replaces an organization's override (superadmins only). A
// nil field goes back to the server's default; 0 means no limit.
func (s *Service) SetOrgQuota(ctx context.Context, actor Actor, org string, o store.OrgQuota) (QuotaUsage, error) {
	if actor.Kind != store.KindHuman || !s.IsSuperadmin(ctx, actor) {
		return QuotaUsage{}, errf(CodePermission, "only superadmins change quotas; ask one to run vx-server quota %s, or to use Admin > Organizations > %s", org, org)
	}
	if _, err := s.loadOrg(ctx, nil, actor, org); err != nil {
		return QuotaUsage{}, err
	}
	return s.setOrgQuota(ctx, &actor.ID, org, o)
}

// ConsoleSetOrgQuota is SetOrgQuota from the server console (vx-server
// quota), with no actor.
func (s *Service) ConsoleSetOrgQuota(ctx context.Context, org string, o store.OrgQuota) (QuotaUsage, error) {
	return s.setOrgQuota(ctx, nil, org, o)
}

func (s *Service) setOrgQuota(ctx context.Context, by *uuid.UUID, handle string, o store.OrgQuota) (QuotaUsage, error) {
	org, err := s.Store.OrgByHandle(ctx, strings.ToLower(strings.TrimSpace(handle)))
	if err != nil {
		return QuotaUsage{}, classify(err)
	}
	for _, v := range []*int64{o.Repos, o.StoredBytes, o.ImportBytes, o.PushBytes, o.OpenEfforts} {
		if v != nil && *v < 0 {
			return QuotaUsage{}, errf(CodeInvalid, "a quota cannot be negative; 0 means no limit")
		}
	}
	o.OrgID, o.UpdatedBy = org.ID, by
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		before, err := tx.GetOrgQuota(ctx, org.ID)
		if err != nil {
			return err
		}
		if err := tx.SetOrgQuota(ctx, o); err != nil {
			return err
		}
		return tx.LogOp(ctx, nil, by, "org.quota", org.Handle, quotaLog(before), quotaLog(o))
	})
	if err != nil {
		return QuotaUsage{}, classify(err)
	}
	return s.usage(ctx, org)
}

// quotaLog is an override as the op log keeps it: only the set fields.
func quotaLog(o store.OrgQuota) map[string]int64 {
	out := map[string]int64{}
	for name, v := range map[string]*int64{"repos": o.Repos, "stored": o.StoredBytes, "import": o.ImportBytes, "push": o.PushBytes, "open_efforts": o.OpenEfforts} {
		if v != nil {
			out[name] = *v
		}
	}
	return out
}

// limitsFor is the quota in effect for an organization.
func (s *Service) limitsFor(ctx context.Context, orgID uuid.UUID) (Quota, error) {
	o, err := s.Store.GetOrgQuota(ctx, orgID)
	if err != nil {
		return Quota{}, err
	}
	return s.Quotas.Defaults.Effective(o), nil
}

// quotaRefused counts and builds a refusal.
func quotaRefused(quota, format string, args ...any) error {
	metrics.QuotaRefused.Inc(quota)
	return errf(CodeTooMany, format, args...)
}

const raiseHint = "or ask a superadmin to raise the quota (vx-server quota %s, or Admin > Organizations > %s)"

// checkRepoQuota refuses a new repository when the organization has as
// many as its quota allows.
func (s *Service) checkRepoQuota(ctx context.Context, org store.Org) error {
	if lim, err := s.limitsFor(ctx, org.ID); err != nil || lim.Repos <= 0 {
		return err
	}
	u, err := s.usage(ctx, org)
	if err != nil {
		return err
	}
	if lim := u.Limits.Repos; lim > 0 && u.Repos >= lim {
		return quotaRefused("repos", "organization %s has %d of its %d repositories (quota): remove one first, "+raiseHint,
			org.Handle, u.Repos, lim, org.Handle, org.Handle)
	}
	return nil
}

// checkEffortQuota refuses a new effort when the organization has as many
// open efforts as its quota allows.
func (s *Service) checkEffortQuota(ctx context.Context, repo store.Repo) error {
	if lim, err := s.limitsFor(ctx, repo.OrgID); err != nil || lim.OpenEfforts <= 0 {
		return err
	}
	org, err := s.Store.QuotaOrg(ctx, repo.OrgID)
	if err != nil {
		return err
	}
	u, err := s.usage(ctx, org)
	if err != nil {
		return err
	}
	if lim := u.Limits.OpenEfforts; lim > 0 && u.OpenEfforts >= lim {
		return quotaRefused("open_efforts", "organization %s has %d of its %d open efforts (quota): ship or close one first (vx effort close), "+raiseHint,
			org.Handle, u.OpenEfforts, lim, org.Handle, org.Handle)
	}
	return nil
}

// checkPushQuota refuses an upload of n bytes that is larger than one
// upload may be, or that would take the organization's stored data over
// its quota.
func (s *Service) checkPushQuota(ctx context.Context, repo store.Repo, n int64) error {
	lim, err := s.limitsFor(ctx, repo.OrgID)
	if err != nil || (lim.PushBytes <= 0 && lim.StoredBytes <= 0) {
		return err
	}
	org, err := s.Store.QuotaOrg(ctx, repo.OrgID)
	if err != nil {
		return err
	}
	if lim.PushBytes > 0 && n > lim.PushBytes {
		return quotaRefused("push", "this upload is %s and one upload to organization %s may be at most %s (quota): land smaller changes, "+raiseHint,
			FormatBytes(n), org.Handle, FormatBytes(lim.PushBytes), org.Handle, org.Handle)
	}
	if lim.StoredBytes > 0 {
		used, known, err := s.orgStoredBytes(ctx, org.ID)
		if err != nil {
			return err
		}
		if known && used+n > lim.StoredBytes {
			return quotaRefused("stored", "organization %s stores %s of its %s (quota) and this upload adds up to %s: remove repositories it no longer needs, "+raiseHint,
				org.Handle, FormatBytes(used), FormatBytes(lim.StoredBytes), FormatBytes(n), org.Handle, org.Handle)
		}
	}
	return nil
}

// RepoSizer is implemented by a git bridge that can ask a git host how
// big a repository is before importing it. known is false when the host
// does not say.
type RepoSizer interface {
	RepoSize(ctx context.Context, repo store.Repo) (bytes int64, known bool, err error)
}

// checkImportQuota asks the host for the repository's size before vx
// downloads it, where the host tells (GitHub, GitLab, Gitea, Forgejo,
// Bitbucket), and refuses one above the organization's import quota or
// its remaining stored data. Plain git remotes cannot be asked; their
// imports are only bounded by the stored data quota afterwards.
func (s *Service) checkImportQuota(ctx context.Context, org store.Org, repo store.Repo) error {
	lim, err := s.limitsFor(ctx, org.ID)
	if err != nil || (lim.ImportBytes <= 0 && lim.StoredBytes <= 0) {
		return err
	}
	sizer, ok := s.Upstream.(RepoSizer)
	if !ok {
		return nil
	}
	size, known, err := sizer.RepoSize(ctx, repo)
	if err != nil || !known {
		return nil // the import itself reports an unreachable host
	}
	if lim.ImportBytes > 0 && size > lim.ImportBytes {
		return quotaRefused("import", "%s is %s on %s and organization %s may import at most %s at once (quota): nothing was downloaded; "+raiseHint,
			repo.Remote, FormatBytes(size), repo.HostName(), org.Handle, FormatBytes(lim.ImportBytes), org.Handle, org.Handle)
	}
	if lim.StoredBytes > 0 {
		used, known, err := s.orgStoredBytes(ctx, org.ID)
		if err != nil {
			return err
		}
		if known && used+size > lim.StoredBytes {
			return quotaRefused("stored", "%s is %s on %s and organization %s stores %s of its %s (quota): nothing was downloaded; remove repositories it no longer needs, "+raiseHint,
				repo.Remote, FormatBytes(size), repo.HostName(), org.Handle, FormatBytes(used), FormatBytes(lim.StoredBytes), org.Handle, org.Handle)
		}
	}
	return nil
}

// DiskSpace is the free space of the data directory's disk.
type DiskSpace struct {
	Path        string
	Free, Total int64
	FreePercent int
	Low         bool // below QuotaSettings.DiskWarnPercent
}

// DataDisk reports the data directory's disk space for the admin pages;
// ok is false without a data directory or when it cannot be read.
func (s *Service) DataDisk() (DiskSpace, bool) {
	dir := s.Quotas.DataDir
	if dir == "" {
		return DiskSpace{}, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return DiskSpace{}, false
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	if total <= 0 {
		return DiskSpace{}, false
	}
	warn := s.Quotas.DiskWarnPercent
	if warn <= 0 {
		warn = 10
	}
	pct := int(free * 100 / total)
	return DiskSpace{Path: dir, Free: free, Total: total, FreePercent: pct, Low: pct < warn}, true
}

// ParseBytes reads a size like 500MB, 10GB, 2GiB or 1048576; "0", "none"
// and "unlimited" are 0 (no limit). Units are powers of 1024 either way:
// what disks and git hosts show.
func ParseBytes(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "", "0", "none", "unlimited", "off":
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"tib", 1 << 40}, {"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"tb", 1 << 40}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || f*float64(mult) > math.MaxInt64/2 {
		return 0, fmt.Errorf("size %q: write a number with an optional unit, like 500MB or 10GB", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatBytes shows a size with one unit, like 1.5 GB.
func FormatBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64) + " " + units[i]
}
