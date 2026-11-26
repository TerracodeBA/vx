package service

// The data processing agreement (v0.5, #41). On vx Cloud the agreement is
// concluded when an owner of the customer's organization accepts it in the
// web UI. The operator publishes its versions as files (VX_DPA_DIR); vx
// never writes the text. The step is off unless the server turns it on
// (VX_REQUIRE_DPA), so self-hosted servers never see it.
//
// The rules:
//
//   - A version is announced when it is published and takes effect at
//     least 30 days later (the first version may take effect at once: there
//     is nobody to announce it to). The manifest is refused otherwise.
//   - An organization may be used for work (requireAgreement) only while it
//     has accepted the version in effect. Until a new version takes effect
//     the earlier acceptance still counts; from its effective date the
//     organization is refused work until an owner accepts it.
//   - Only a person who owns the organization accepts, in the web UI. Not
//     agents, not other members, not superadmins who are not owners.
//   - Each acceptance records the version, the SHA-256 of its text, who,
//     when and from which address; the text itself is kept once by hash.
//   - Reads never need it, so people can see what is missing.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// AgreementSettings configures the data processing agreement step.
type AgreementSettings struct {
	// Required turns the step on (VX_REQUIRE_DPA). Off, nothing asks for
	// an agreement and nothing shows one.
	Required bool
	// Dir is where the versions came from (VX_DPA_DIR), for messages.
	Dir string
	// Versions are the published versions (LoadAgreementVersions), by
	// effective date.
	Versions []AgreementVersion
}

// AgreementVersion is one version of the agreement, as the operator
// published it.
type AgreementVersion struct {
	ID        string
	Title     string
	File      string // the file name in the directory
	Published time.Time
	Effective time.Time
	Text      []byte // Markdown
	SHA256    string // of Text, hex
}

// MinAgreementNotice is how long before it takes effect a new version must
// be published.
const MinAgreementNotice = 30 * 24 * time.Hour

// ErrAgreementRequired is wrapped by the error of an operation refused
// because the organization has not accepted the agreement in effect.
var ErrAgreementRequired = errors.New("data processing agreement not accepted")

// maxAgreementText bounds one version's file.
const maxAgreementText = 1 << 20

var agreementIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// agreementManifest is manifest.json in VX_DPA_DIR.
type agreementManifest struct {
	Versions []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		File      string `json:"file"`
		Published string `json:"published"`
		Effective string `json:"effective"`
	} `json:"versions"`
}

// AgreementManifest is the name of the manifest in VX_DPA_DIR.
const AgreementManifest = "manifest.json"

// LoadAgreementVersions reads the versions from dir: manifest.json lists
// each version's id, file, publication and effective date; each file is
// the Markdown text of one version. It refuses a manifest with a missing
// or empty file, a duplicate id or effective date, or a new version
// published less than 30 days before it takes effect.
func LoadAgreementVersions(dir string) ([]AgreementVersion, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("no directory given: set VX_DPA_DIR to the directory with manifest.json and one Markdown file per version")
	}
	raw, err := os.ReadFile(filepath.Join(dir, AgreementManifest))
	if err != nil {
		return nil, fmt.Errorf("read the manifest: %w (VX_DPA_DIR needs a %s listing the versions)", err, AgreementManifest)
	}
	var m agreementManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", AgreementManifest, err)
	}
	if len(m.Versions) == 0 {
		return nil, fmt.Errorf("%s lists no versions", AgreementManifest)
	}
	ids := map[string]bool{}
	out := make([]AgreementVersion, 0, len(m.Versions))
	for i, mv := range m.Versions {
		where := fmt.Sprintf("%s, version %d", AgreementManifest, i+1)
		if mv.ID != "" {
			where = fmt.Sprintf("%s, version %s", AgreementManifest, mv.ID)
		}
		if !agreementIDRe.MatchString(mv.ID) {
			return nil, fmt.Errorf("%s: id %q: use 1 to 64 letters, digits, dots, dashes or underscores, like 2026-12", where, mv.ID)
		}
		if ids[mv.ID] {
			return nil, fmt.Errorf("%s: the id is listed twice; every version needs its own id", where)
		}
		ids[mv.ID] = true
		if mv.File == "" || mv.File != filepath.Base(mv.File) || strings.HasPrefix(mv.File, ".") {
			return nil, fmt.Errorf("%s: file %q: give the name of a file in VX_DPA_DIR, like %s.md", where, mv.File, mv.ID)
		}
		v := AgreementVersion{ID: mv.ID, Title: strings.TrimSpace(mv.Title), File: mv.File}
		if v.Published, err = parseAgreementDate(mv.Published); err != nil {
			return nil, fmt.Errorf("%s: published: %w", where, err)
		}
		if v.Effective, err = parseAgreementDate(mv.Effective); err != nil {
			return nil, fmt.Errorf("%s: effective: %w", where, err)
		}
		if v.Effective.Before(v.Published) {
			return nil, fmt.Errorf("%s: it takes effect (%s) before it is published (%s)", where, agreementDay(v.Effective), agreementDay(v.Published))
		}
		path := filepath.Join(dir, mv.File)
		st, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if !st.Mode().IsRegular() || st.Size() > maxAgreementText {
			return nil, fmt.Errorf("%s: %s must be a regular file of at most 1 MiB", where, mv.File)
		}
		if v.Text, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if len(bytes.TrimSpace(v.Text)) == 0 {
			return nil, fmt.Errorf("%s: %s is empty", where, mv.File)
		}
		if !utf8.Valid(v.Text) {
			return nil, fmt.Errorf("%s: %s is not UTF-8 text", where, mv.File)
		}
		sum := sha256.Sum256(v.Text)
		v.SHA256 = hex.EncodeToString(sum[:])
		if v.Title == "" {
			v.Title = "Data processing agreement, version " + v.ID
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Effective.Before(out[j].Effective) })
	for i := 1; i < len(out); i++ {
		prev, v := out[i-1], out[i]
		if v.Effective.Equal(prev.Effective) {
			return nil, fmt.Errorf("%s: versions %s and %s take effect at the same time (%s)", AgreementManifest, prev.ID, v.ID, agreementDay(v.Effective))
		}
		if notice := v.Effective.Sub(v.Published); notice < MinAgreementNotice {
			return nil, fmt.Errorf("%s: version %s is published %s and takes effect %s, %d days later; a new version must be published at least 30 days before it takes effect, so owners can read it first",
				AgreementManifest, v.ID, agreementDay(v.Published), agreementDay(v.Effective), int(notice.Hours()/24))
		}
	}
	return out, nil
}

// parseAgreementDate reads a date (2026-12-01, midnight UTC) or an RFC 3339
// time.
func parseAgreementDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date: use YYYY-MM-DD (midnight UTC) or an RFC 3339 time", s)
}

func agreementDay(t time.Time) string {
	if t.Equal(t.Truncate(24 * time.Hour)) {
		return t.UTC().Format(time.DateOnly)
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// AgreementRequired reports whether this server asks organizations to
// accept the data processing agreement.
func (s *Service) AgreementRequired() bool { return s.Agreement.Required }

// CheckAgreements is run at start with the step on: a version must be in
// effect, and no published text may differ from one an owner accepted
// under the same id (a changed text is a new version).
func (s *Service) CheckAgreements(ctx context.Context) error {
	if !s.Agreement.Required {
		return nil
	}
	if len(s.Agreement.Versions) == 0 {
		return errors.New("VX_REQUIRE_DPA is on but no version of the data processing agreement is configured: set VX_DPA_DIR")
	}
	now := s.Clock()
	if cur := s.currentAgreement(now); cur == nil {
		first := s.Agreement.Versions[0]
		return fmt.Errorf("VX_REQUIRE_DPA is on but no version of the data processing agreement is in effect yet: the earliest, %s, takes effect %s. Start the server then, or publish a version that is in effect", first.ID, agreementDay(first.Effective))
	}
	hashes, err := s.Store.AcceptedAgreementHashes(ctx)
	if err != nil {
		return err
	}
	for _, v := range s.Agreement.Versions {
		for _, h := range hashes[v.ID] {
			if h != v.SHA256 {
				return fmt.Errorf("the text of version %s of the data processing agreement (%s) differs from the one owners accepted (sha256 %s, now %s). Put the accepted text back and publish the change as a new version", v.ID, v.File, h, v.SHA256)
			}
		}
	}
	return nil
}

// currentAgreement is the version in effect at t: the last one whose
// effective date has come. nil before the first.
func (s *Service) currentAgreement(t time.Time) *AgreementVersion {
	var cur *AgreementVersion
	for i := range s.Agreement.Versions {
		if v := &s.Agreement.Versions[i]; !v.Effective.After(t) {
			cur = v
		}
	}
	return cur
}

// announcedAgreements are the versions published by t that are not in
// effect yet, by effective date.
func (s *Service) announcedAgreements(t time.Time) []AgreementVersion {
	var out []AgreementVersion
	for _, v := range s.Agreement.Versions {
		if !v.Published.After(t) && v.Effective.After(t) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Service) agreementVersion(id string) (AgreementVersion, bool) {
	for _, v := range s.Agreement.Versions {
		if v.ID == id {
			return v, true
		}
	}
	return AgreementVersion{}, false
}

// agreementGap is what an organization lacks at a time: nil when it has
// accepted the version in effect (or the step is off).
type agreementGap struct {
	version  AgreementVersion
	accepted []store.OrgAgreement // earlier acceptances, newest first
}

func (s *Service) agreementGapOf(ctx context.Context, org uuid.UUID) (*agreementGap, error) {
	if !s.Agreement.Required {
		return nil, nil
	}
	cur := s.currentAgreement(s.Clock())
	if cur == nil {
		return nil, errf(CodeUnavailable, "no version of the data processing agreement is in effect on this server; the operator publishes one (VX_DPA_DIR)")
	}
	acc, err := s.Store.OrgAgreements(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, a := range acc {
		if a.VersionID == cur.ID {
			return nil, nil
		}
	}
	return &agreementGap{version: *cur, accepted: acc}, nil
}

// requireAgreement refuses work in repo's organization while it has not
// accepted the agreement in effect. what names the work ("starting
// efforts"). Call it after Authorize, so callers without a role still see
// "not found".
func (s *Service) requireAgreement(ctx context.Context, repo store.Repo, what string) error {
	if !s.Agreement.Required {
		return nil
	}
	o, err := s.orgOfRepo(ctx, repo)
	if err != nil {
		return err
	}
	return s.requireOrgAgreement(ctx, o, what)
}

func (s *Service) orgOfRepo(ctx context.Context, repo store.Repo) (store.Org, error) {
	if repo.Org != "" {
		if o, err := s.Store.OrgByHandle(ctx, repo.Org); err == nil && o.ID == repo.OrgID {
			return o, nil
		}
	}
	r, err := s.Store.RepoByID(ctx, repo.ID)
	if err != nil {
		return store.Org{}, classify(err)
	}
	o, err := s.Store.OrgByHandle(ctx, r.Org)
	return o, classify(err)
}

// requireOrgAgreement is requireAgreement for an organization.
func (s *Service) requireOrgAgreement(ctx context.Context, o store.Org, what string) error {
	gap, err := s.agreementGapOf(ctx, o.ID)
	if err != nil || gap == nil {
		return err
	}
	return s.agreementError(ctx, o, gap, what)
}

// agreementOK reports whether work may happen in an organization, for
// background imports that have nobody to tell.
func (s *Service) agreementOK(ctx context.Context, org uuid.UUID) bool {
	gap, err := s.agreementGapOf(ctx, org)
	return err == nil && gap == nil
}

func (s *Service) agreementError(ctx context.Context, o store.Org, gap *agreementGap, what string) error {
	kind := "organization " + o.Handle
	if o.Personal {
		kind = "personal organization " + o.Handle
	}
	var msg string
	if len(gap.accepted) == 0 {
		msg = fmt.Sprintf("%s has not accepted the data processing agreement, so it cannot be used for work (%s) yet", kind, what)
	} else {
		msg = fmt.Sprintf("version %s of the data processing agreement took effect %s and %s has not accepted it, so it cannot be used for work (%s) until it does", gap.version.ID, agreementDay(gap.version.Effective), kind, what)
	}
	where := "Organization > Agreement"
	if u := s.publicURL(); u != "" {
		where += " (" + u + "/ui/orgs/" + o.Handle + "#agreement)"
	} else {
		where += " (/ui/orgs/" + o.Handle + "#agreement)"
	}
	msg += fmt.Sprintf(". An owner of %s must accept the data processing agreement: %s, in the web UI. Owners: %s", o.Handle, where, s.ownerHandles(ctx, o.ID))
	return &Error{Code: CodeFailedPrecondition, Msg: msg, Err: ErrAgreementRequired}
}

// ownerHandles names up to five owners of an organization, for messages.
func (s *Service) ownerHandles(ctx context.Context, org uuid.UUID) string {
	members, err := s.Store.ListOrgMembers(ctx, org)
	if err != nil {
		return "unknown"
	}
	var out []string
	for _, m := range members {
		if OrgRole(m.Role) == OrgOwner {
			out = append(out, m.Person.Handle)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	if len(out) > 5 {
		out = append(out[:5], "...")
	}
	return strings.Join(out, ", ")
}

// AgreementRecord is one acceptance with the version it was for.
type AgreementRecord struct {
	store.OrgAgreement
	Title string // the version's title, when the server still publishes it
	Known bool   // the server still publishes this version
}

// AgreementView is an organization's agreement status.
type AgreementView struct {
	Required bool
	Org      Org
	MyRole   OrgRole
	// CanAccept: the caller is a person who owns the organization.
	CanAccept bool
	Owners    string // up to five owners' handles
	// Current is the version in effect; CurrentAccepted its acceptance,
	// nil while it is missing. Usable is CurrentAccepted != nil.
	Current         *AgreementVersion
	CurrentAccepted *AgreementRecord
	Usable          bool
	// Announced are versions published and not in effect yet; accepted
	// ones have an acceptance in AnnouncedAccepted (by id).
	Announced         []AgreementVersion
	AnnouncedAccepted map[string]AgreementRecord
	// Pending is what an owner should accept next: the version in effect
	// while it is missing, else the first announced one not accepted yet.
	Pending *AgreementVersion
	// Missing is the refusal work gets while it is not Usable.
	Missing string
	// History is every acceptance, newest first.
	History []AgreementRecord
	// AcceptURL is where an owner accepts.
	AcceptURL string
}

// OrgAgreement returns an organization's agreement status (members of the
// organization and superadmins; anyone else gets not found). With the step
// off, only Required (false) is set.
func (s *Service) OrgAgreement(ctx context.Context, actor Actor, org string) (AgreementView, error) {
	a, err := s.loadOrg(ctx, nil, actor, org)
	if err != nil {
		return AgreementView{}, err
	}
	v := AgreementView{Required: s.Agreement.Required, Org: orgFromStore(a.org), MyRole: a.role}
	if !v.Required {
		return v, nil
	}
	v.CanAccept = actor.Kind == store.KindHuman && a.isOwner()
	v.Owners = s.ownerHandles(ctx, a.org.ID)
	v.AcceptURL = s.publicURL() + "/ui/orgs/" + a.org.Handle + "#agreement"
	acc, err := s.Store.OrgAgreements(ctx, a.org.ID)
	if err != nil {
		return AgreementView{}, err
	}
	byID := map[string]AgreementRecord{}
	for _, x := range acc {
		ver, known := s.agreementVersion(x.VersionID)
		rec := AgreementRecord{OrgAgreement: x, Title: ver.Title, Known: known}
		v.History = append(v.History, rec)
		byID[x.VersionID] = rec
	}
	now := s.Clock()
	if cur := s.currentAgreement(now); cur != nil {
		c := *cur
		v.Current = &c
		if rec, ok := byID[c.ID]; ok {
			v.CurrentAccepted, v.Usable = &rec, true
		} else {
			v.Pending = &c
		}
	}
	v.AnnouncedAccepted = map[string]AgreementRecord{}
	for _, an := range s.announcedAgreements(now) {
		v.Announced = append(v.Announced, an)
		if rec, ok := byID[an.ID]; ok {
			v.AnnouncedAccepted[an.ID] = rec
		} else if v.Pending == nil {
			p := an
			v.Pending = &p
		}
	}
	if !v.Usable && v.Current != nil {
		gap := &agreementGap{version: *v.Current, accepted: acc}
		v.Missing = s.agreementError(ctx, a.org, gap, "connecting or importing repositories, starting efforts, opening workspaces, landing, shipping").Error()
	}
	return v, nil
}

// AgreementText returns a published version (in effect, announced or
// earlier) for reading and download (members and superadmins).
func (s *Service) AgreementText(ctx context.Context, actor Actor, org, version string) (AgreementVersion, error) {
	if _, err := s.loadOrg(ctx, nil, actor, org); err != nil {
		return AgreementVersion{}, err
	}
	if !s.Agreement.Required {
		return AgreementVersion{}, errf(CodeFailedPrecondition, "this server does not ask for a data processing agreement")
	}
	now := s.Clock()
	if version == "" {
		cur := s.currentAgreement(now)
		if cur == nil {
			return AgreementVersion{}, errf(CodeNotFound, "no version of the data processing agreement is in effect")
		}
		return *cur, nil
	}
	v, ok := s.agreementVersion(version)
	if !ok || v.Published.After(now) {
		return AgreementVersion{}, errf(CodeNotFound, "the data processing agreement has no version %q on this server", version)
	}
	return v, nil
}

// AcceptedAgreementText returns an acceptance of org and the text exactly
// as it was accepted (members and superadmins).
func (s *Service) AcceptedAgreementText(ctx context.Context, actor Actor, org, id string) (AgreementRecord, []byte, error) {
	a, err := s.loadOrg(ctx, nil, actor, org)
	if err != nil {
		return AgreementRecord{}, nil, err
	}
	uid, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return AgreementRecord{}, nil, errf(CodeNotFound, "organization %s has no acceptance %q", a.org.Handle, id)
	}
	x, err := s.Store.OrgAgreementByID(ctx, a.org.ID, uid)
	if err != nil {
		return AgreementRecord{}, nil, classify(err)
	}
	text, err := s.Store.AgreementText(ctx, x.SHA256)
	if err != nil {
		return AgreementRecord{}, nil, classify(err)
	}
	ver, known := s.agreementVersion(x.VersionID)
	return AgreementRecord{OrgAgreement: x, Title: ver.Title, Known: known}, text, nil
}

// AcceptAgreement records that the calling person, an owner of org,
// accepts a version of the agreement: the one in effect or an announced
// one. sha256 is the hash of the text they were shown; a text that changed
// since is refused. The web UI is the only caller: acceptance is a
// deliberate act by a person, so there is no API or CLI for it.
func (s *Service) AcceptAgreement(ctx context.Context, actor Actor, org, version, sum string) (AgreementRecord, error) {
	if !s.Agreement.Required {
		return AgreementRecord{}, errf(CodeFailedPrecondition, "this server does not ask for a data processing agreement")
	}
	if actor.Kind != store.KindHuman {
		who := actor.Ref()
		if actor.Parent != nil {
			who += " is an agent acting for " + actor.Parent.Ref() + ", who"
		}
		return AgreementRecord{}, errf(CodePermission, "only a person who owns %s accepts the data processing agreement, in the web UI; %s must sign in as themselves", org, who)
	}
	now := s.Clock()
	v, ok := s.agreementVersion(strings.TrimSpace(version))
	if !ok || v.Published.After(now) {
		return AgreementRecord{}, errf(CodeNotFound, "the data processing agreement has no version %q on this server", version)
	}
	if cur := s.currentAgreement(now); cur != nil && v.Effective.Before(cur.Effective) {
		return AgreementRecord{}, errf(CodeFailedPrecondition, "version %s is no longer in effect; accept version %s, which is", v.ID, cur.ID)
	}
	if strings.TrimSpace(sum) != v.SHA256 {
		return AgreementRecord{}, errf(CodeFailedPrecondition, "the text of version %s changed since you opened it; read it again and accept", v.ID)
	}
	var out store.OrgAgreement
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		a, err := s.loadOrg(ctx, tx, actor, org)
		if err != nil {
			return err
		}
		if !a.isOwner() {
			if a.role == "" {
				return errf(CodePermission, "superadmins cannot accept the data processing agreement for an organization; an owner of %s accepts it. Owners: %s", a.org.Handle, s.ownerHandles(ctx, a.org.ID))
			}
			return errf(CodePermission, "only owners of %s accept the data processing agreement; %s is %s. Owners: %s", a.org.Handle, actor.Ref(), a.role, s.ownerHandles(ctx, a.org.ID))
		}
		var created bool
		out, created, err = tx.AcceptOrgAgreement(ctx, store.OrgAgreement{
			OrgID: a.org.ID, VersionID: v.ID, SHA256: v.SHA256, AcceptedBy: &actor.ID, AcceptedByRef: actor.Ref(),
			AcceptedAt: now, IP: clientIP(ctx),
		}, v.Text)
		if err != nil || !created {
			return err
		}
		return tx.LogOp(ctx, nil, &actor.ID, "org.dpa_accept", a.org.Handle, nil, map[string]any{
			"version": v.ID, "sha256": v.SHA256, "effective": v.Effective, "ip": out.IP,
		})
	})
	if err != nil {
		return AgreementRecord{}, classify(err)
	}
	return AgreementRecord{OrgAgreement: out, Title: v.Title, Known: true}, nil
}

// AgreementNotice is an organization a person owns that needs the
// agreement accepted: one in effect (Blocking: no work until accepted) or
// an announced one.
type AgreementNotice struct {
	Org      string
	Personal bool
	Version  AgreementVersion
	Blocking bool
	At       time.Time // when it asked for attention: published, or in effect
}

// AgreementNotices lists the notices for the organizations the caller (a
// person) owns. Agents and other members get none: they cannot accept.
// Personal organizations without repositories get none either: they are
// accepted for on first use.
func (s *Service) AgreementNotices(ctx context.Context, actor Actor) ([]AgreementNotice, error) {
	if !s.Agreement.Required || actor.Kind != store.KindHuman {
		return nil, nil
	}
	rows, err := s.Store.ListOrgs(ctx, actor.ID, false)
	if err != nil {
		return nil, err
	}
	var owned []store.Org
	var ids []uuid.UUID
	for _, r := range rows {
		if OrgRole(r.Role) == OrgOwner {
			owned = append(owned, r.Org)
			ids = append(ids, r.Org.ID)
		}
	}
	acc, err := s.Store.OrgAgreementsOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	used, err := s.Store.OrgsWithRepos(ctx, ids)
	if err != nil {
		return nil, err
	}
	have := map[uuid.UUID][]string{}
	for _, a := range acc {
		have[a.OrgID] = append(have[a.OrgID], a.VersionID)
	}
	now := s.Clock()
	cur := s.currentAgreement(now)
	announced := s.announcedAgreements(now)
	var out []AgreementNotice
	for _, o := range owned {
		if o.Personal && !used[o.ID] {
			// A personal organization is accepted for on first use: the
			// refusal of its first repository says so. No nagging before.
			continue
		}
		if cur != nil && !slices.Contains(have[o.ID], cur.ID) {
			at := cur.Effective
			if o.CreatedAt.After(at) {
				at = o.CreatedAt
			}
			out = append(out, AgreementNotice{Org: o.Handle, Personal: o.Personal, Version: *cur, Blocking: true, At: at})
			continue // the one in effect first; announced ones after it
		}
		for _, an := range announced {
			if !slices.Contains(have[o.ID], an.ID) {
				out = append(out, AgreementNotice{Org: o.Handle, Personal: o.Personal, Version: an, At: an.Published})
				break
			}
		}
	}
	return out, nil
}

// ClientIPFrom is the caller's address recorded with WithClientIP, or "".
func ClientIPFrom(ctx context.Context) string { return clientIP(ctx) }

// AgreementDay formats an agreement date for pages and the CLI.
func AgreementDay(t time.Time) string { return agreementDay(t) }

// PublishedDay and EffectiveDay are the version's dates, for pages.
func (v AgreementVersion) PublishedDay() string { return agreementDay(v.Published) }

// EffectiveDay is when the version takes effect, for pages.
func (v AgreementVersion) EffectiveDay() string { return agreementDay(v.Effective) }
