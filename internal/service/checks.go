package service

// Company-defined checks (#47): results (evidence) of the checks a
// repository's vx.toml or its organization defines, bound to a change and
// the exact tree they describe, and the gates that refuse a land or a ship
// until every blocking check has a passing result from a reporter it
// allows. vx stores and enforces; agents, CI and people check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
	"github.com/TerracodeBA/vx/internal/store"
)

// CheckSettings bound what one report may carry (VX_CHECK_ARTIFACT_MAX,
// VX_CHECK_REPORT_MAX). Zero fields take the defaults.
type CheckSettings struct {
	MaxArtifactBytes int64 // one artifact, the log or the data
	MaxReportBytes   int64 // everything one report sends
}

// Defaults of CheckSettings; one report stays under the API's message
// size.
const (
	DefaultCheckArtifactBytes = 20 << 20
	DefaultCheckReportBytes   = 48 << 20
)

func (c CheckSettings) artifactMax() int64 {
	if c.MaxArtifactBytes > 0 {
		return c.MaxArtifactBytes
	}
	return DefaultCheckArtifactBytes
}

func (c CheckSettings) reportMax() int64 {
	if c.MaxReportBytes > 0 {
		return c.MaxReportBytes
	}
	return DefaultCheckReportBytes
}

// Check states, as vx check status shows them.
const (
	CheckStateMissing   = "missing"
	CheckStatePending   = "pending"
	CheckStatePassed    = "passed"
	CheckStateFailed    = "failed"
	CheckStateNeutral   = "neutral"
	CheckStateNotNeeded = "not needed" // the change touches none of the check's paths
)

// ArtifactInput is one file a report attaches.
type ArtifactInput struct {
	Name        string // a file name, no directories, e.g. flow.gif
	ContentType string // empty: from the name, else sniffed
	Data        []byte
}

// ReportInput is one result of a check.
type ReportInput struct {
	Repo   string
	Effort string
	// ChangeID and Tree say what the result describes. An empty ChangeID
	// is the effort head; a zero Tree is the change's newest tree on the
	// effort (an open workspace has none yet: pass the snapshot's tree).
	ChangeID string
	Tree     hash.Hash
	Name     string // the check, as vx.toml names it
	Status   string // pass, fail, neutral or pending
	Summary  string
	Log      []byte
	// Artifacts are files the check produced: screenshots, recordings,
	// reports. They become objects of the repository.
	Artifacts []ArtifactInput
	// Data is structured output (JSON), DataName the file name it came
	// from (e.g. steps.json); both may be empty.
	Data     []byte
	DataName string
	// How is the command of a run check, set when vx ran it.
	How                string
	Model              string
	Started, Finished  time.Time
	CITokenID          *uuid.UUID // set by the server for CI service tokens, never by a client
	fromLand, isReview bool
}

// CheckResult is a stored result with where it belongs and, where it is
// read for a check of a change, whether it counts for that check.
type CheckResult struct {
	store.CheckResult
	RepoName   string
	EffortSlug string
	Counts     bool   // its reporter is one the check allows (resultCounts)
	Why        string // why it does not count; empty when it does
}

// reporterCounts applies the check's reporter rule (ReporterAllowed,
// checktrust.go) to who reported r and who made the change.
func (s *Service) reporterCounts(ctx context.Context, def rules.CheckDef, change ChangeAuthor, r store.CheckResult) (bool, string) {
	return ReporterAllowed(string(def.Reporter), def.AllowSamePerson, change, reporterOf(r))
}

// reporterOf is who reported a stored result.
func reporterOf(r store.CheckResult) ReporterIdentity {
	id := ReporterIdentity{Principal: r.Reporter}
	switch {
	case r.CITokenID != nil:
		id.Kind = ReporterIsCI
	case strings.HasPrefix(r.Reporter, "user:"):
		id.Kind, id.Person = ReporterIsPerson, r.Reporter
	case strings.HasPrefix(r.Reporter, "agent:"):
		id.Kind, id.Person = ReporterIsAgent, r.OnBehalfOf
	default:
		id.Kind = ReporterIsService
	}
	return id
}

// resultCounts decides whether a result counts for a check: a run check
// (run = "CMD") needs the result to say vx ran exactly its command (vx
// land --run or vx check run); then the check's reporter rule decides
// (reporterCounts).
func (s *Service) resultCounts(ctx context.Context, def rules.CheckDef, change ChangeAuthor, r store.CheckResult) (bool, string) {
	if def.IsRun() && strings.TrimSpace(r.How) != def.Run {
		return false, fmt.Sprintf("reported, not run by vx: vx check run %s (or vx land --run %s)", def.Name, rules.ShellQuote(def.Run))
	}
	return s.reporterCounts(ctx, def, change, r)
}

// checkTarget is what a result describes: a change and the tree.
type checkTarget struct {
	Repo     store.Repo
	Effort   store.Effort
	ChangeID string
	Tree     hash.Hash
	// BaseTree is what the change is compared with to see which checks
	// apply: the workspace's base, or the landed change's parent.
	BaseTree hash.Hash
	// EffortBaseTree is the effort's base, for the checks that apply to
	// what ships; zero unless Head.
	EffortBaseTree hash.Hash
	Author         ChangeAuthor
	Head           bool // the effort head
	Landed         bool // a changeset of the effort (not an open workspace)
}

// resolveCheckTarget finds the change and tree a report or a status is
// about; see ReportInput.
func (s *Service) resolveCheckTarget(ctx context.Context, repo store.Repo, eff store.Effort, changeID string, tree hash.Hash) (checkTarget, error) {
	t := checkTarget{Repo: repo, Effort: eff}
	head, err := s.Store.GetChangeset(ctx, repo.ID, eff.Head)
	if err != nil {
		return t, classify(err)
	}
	effortBase := func() hash.Hash {
		if b, err := s.Store.GetChangeset(ctx, repo.ID, eff.Base); err == nil {
			return b.Tree
		}
		return hash.Zero
	}
	parentTree := func(cs store.ChangesetRow) hash.Hash {
		if len(cs.Parents) > 0 {
			if p, err := s.Store.GetChangeset(ctx, repo.ID, cs.Parents[0]); err == nil {
				return p.Tree
			}
		}
		return hash.Zero
	}
	headTarget := func() checkTarget {
		t.ChangeID, t.Tree, t.BaseTree, t.Head, t.Landed = head.ChangeID, head.Tree, parentTree(head), true, true
		t.EffortBaseTree = effortBase()
		t.Author = authorOf(head.Author.Principal, head.Author.OnBehalfOf)
		return t
	}
	if changeID == "" {
		if !tree.IsZero() && tree != head.Tree {
			return t, errf(CodeInvalid, "tree %s is not the head of effort %s; name its change (--change ID)", tree.Short(), eff.Slug)
		}
		return headTarget(), nil
	}
	if changeID == head.ChangeID && (tree.IsZero() || tree == head.Tree) {
		return headTarget(), nil
	}
	// An open workspace of the effort: its snapshot, before it lands.
	wss, err := s.Store.ListWorkspaces(ctx, eff.ID, store.WorkspaceOpen)
	if err != nil {
		return t, err
	}
	for _, ws := range wss {
		if ws.ChangeID != changeID && !(len(changeID) >= 4 && strings.HasPrefix(ws.ChangeID, changeID)) {
			continue
		}
		base, err := s.Store.GetChangeset(ctx, repo.ID, ws.Base)
		if err != nil {
			return t, classify(err)
		}
		if tree.IsZero() {
			if r, err := s.Store.PendingReviewOf(ctx, ws.ID); err == nil {
				tree = r.Tree
			} else {
				return t, errf(CodeInvalid, "change %s has not landed yet, so vx does not know its files: report from its working copy (vx check report runs there with its snapshot), or pass the tree it was checked on (--tree)", ws.ChangeID)
			}
		}
		if err := s.checkInRepo(ctx, repo, base.Tree, tree); err != nil {
			return t, err
		}
		owner, err := s.Store.PrincipalByID(ctx, ws.OwnerID)
		if err != nil {
			return t, classify(err)
		}
		a, err := s.ResolveActor(ctx, owner)
		if err != nil {
			return t, err
		}
		t.ChangeID, t.Tree, t.BaseTree = ws.ChangeID, tree, base.Tree
		t.Author = authorOf(a.Ref(), a.OnBehalfOf())
		return t, nil
	}
	// A landed change: its newest version, or the version whose tree (or
	// the snapshot the server merged) is the tree given.
	cs, err := s.Store.ResolveChangeset(ctx, repo.ID, changeID)
	if err != nil || cs.EffortID == nil || *cs.EffortID != eff.ID {
		return t, errf(CodeNotFound, "no change %s in effort %s: vx log lists its changes, vx status shows your workspace", changeID, eff.Slug)
	}
	if !tree.IsZero() && tree != cs.Tree && tree != cs.SnapshotTree {
		return t, errf(CodeInvalid, "tree %s is not a tree of change %s (it has %s); report on the change's tree, or on the effort head without --change", tree.Short(), cs.ChangeID, cs.Tree.Short())
	}
	if tree.IsZero() {
		tree = cs.Tree
	}
	t.ChangeID, t.Tree, t.BaseTree, t.Landed = cs.ChangeID, tree, parentTree(cs), true
	if cs.Hash == head.Hash && tree == head.Tree {
		t.Head, t.EffortBaseTree = true, effortBase()
	}
	t.Author = authorOf(cs.Author.Principal, cs.Author.OnBehalfOf)
	return t, nil
}

// checkDefs are the checks that apply to trees of repo: the checks of
// each tree (repoCheckDefs; a change can add checks for itself but never
// drop one the effort has), the same check taken once. Rules are those of
// the first tree that has a vx.toml that parses.
func (s *Service) checkDefs(ctx context.Context, repo store.Repo, trees ...hash.Hash) ([]rules.CheckDef, rules.Rules, []string) {
	var out []rules.CheckDef
	var first rules.Rules
	var warnings []string
	seen := map[string]bool{}
	for _, t := range trees {
		r, err := rules.At(ctx, s.Blobs, t)
		if err != nil {
			continue
		}
		if first.Source == "" {
			first = r
		}
		defs, ws := s.repoCheckDefs(ctx, repo, r)
		warnings = append(warnings, ws...)
		for _, d := range defs {
			key := string(d.Origin) + "\x00" + d.Name + "\x00" + d.Run + "\x00" + strings.Join(d.Paths, ",")
			if !seen[key] {
				seen[key] = true
				out = append(out, d)
			}
		}
	}
	return out, first, warnings
}

// repoCheckDefs is the one place that says which checks a repository's
// rules r stand for: r.Effective() ([[check]], then [checks] required and
// [[policy]] as checks). Organization rules (#47 part 3) join here; the
// warnings say what was dropped.
func (s *Service) repoCheckDefs(ctx context.Context, repo store.Repo, r rules.Rules) ([]rules.CheckDef, []string) {
	org, err := s.orgChecksOf(ctx, repo)
	if err != nil {
		slog.Default().Warn("organization checks", "repo", repo.Name, "err", err)
	}
	if org.Version == 0 {
		return r.Effective(), nil
	}
	return MergeOrgChecks(org, r.Effective())
}

// ReportCheck stores one result of a check (write): bound to the change
// and the tree in, with its log, artifacts and data as objects of the
// repository.
func (s *Service) ReportCheck(ctx context.Context, actor Actor, in ReportInput) (CheckResult, error) {
	repo, ciToken, err := s.authorizeReport(ctx, actor, in.Repo, in.Name)
	if err != nil {
		return CheckResult{}, err
	}
	in.CITokenID = ciToken
	eff, err := s.Store.GetEffort(ctx, repo.ID, in.Effort)
	if err != nil {
		return CheckResult{}, classify(err)
	}
	switch in.Status {
	case store.CheckPass, store.CheckFail, store.CheckNeutral, store.CheckPending:
	default:
		return CheckResult{}, errf(CodeInvalid, "status %q is not one of pass, fail, neutral or pending", in.Status)
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return CheckResult{}, errf(CodeInvalid, "name the check to report (vx check status lists them)")
	}
	t, err := s.resolveCheckTarget(ctx, repo, eff, in.ChangeID, in.Tree)
	if err != nil {
		return CheckResult{}, err
	}
	head, _ := s.Store.GetChangeset(ctx, repo.ID, eff.Head)
	defs, _, _ := s.checkDefs(ctx, repo, t.Tree, head.Tree)
	def, ok := findCheck(defs, in.Name)
	if !ok {
		return CheckResult{}, errf(CodeNotFound, "no check %q in vx.toml for this change (%s); vx check status lists its checks", in.Name, checkNames(defs))
	}
	if def.Origin == rules.OriginPolicy && def.Name == rules.PolicyReviewCheck && !in.isReview {
		return CheckResult{}, errf(CodeInvalid, "check %q is a person's approval under a human [[policy]]: land, and a person approves with vx review approve", def.Name)
	}
	r, err := s.storeCheckResult(ctx, actor, repo, eff, t, def, in)
	if err != nil {
		return CheckResult{}, err
	}
	out := CheckResult{CheckResult: r, RepoName: repo.Name, EffortSlug: eff.Slug}
	out.Counts, out.Why = s.resultCounts(ctx, def, t.Author, r)
	if out.Counts {
		s.afterCheckResult(ctx, repo, eff.ID, def.Name, r.Tree, r.ID)
	}
	return out, nil
}

func findCheck(defs []rules.CheckDef, name string) (rules.CheckDef, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return rules.CheckDef{}, false
}

func checkNames(defs []rules.CheckDef) string {
	var names []string
	seen := map[string]bool{}
	for _, d := range defs {
		if !seen[d.Name] {
			seen[d.Name] = true
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return "it defines none"
	}
	return "known: " + strings.Join(names, ", ")
}

// storeCheckResult validates in against def, writes its blobs through
// repoWrites and stores the result with an entry in the operation log.
func (s *Service) storeCheckResult(ctx context.Context, actor Actor, repo store.Repo, eff store.Effort, t checkTarget, def rules.CheckDef, in ReportInput) (store.CheckResult, error) {
	max := s.Checks.artifactMax()
	total := int64(len(in.Log) + len(in.Data))
	if int64(len(in.Log)) > max {
		return store.CheckResult{}, errf(CodeInvalid, "the log is %s; one may be at most %s: send its end (or attach it compressed)", FormatBytes(int64(len(in.Log))), FormatBytes(max))
	}
	names := map[string]bool{}
	for i, a := range in.Artifacts {
		a.Name = strings.TrimSpace(a.Name)
		in.Artifacts[i].Name = a.Name
		switch {
		case a.Name == "" || a.Name == "." || a.Name == ".." || strings.ContainsAny(a.Name, "/\\\x00"):
			return store.CheckResult{}, errf(CodeInvalid, "artifact %q: give a file name without directories, e.g. flow.gif", a.Name)
		case names[a.Name]:
			return store.CheckResult{}, errf(CodeInvalid, "artifact %q is attached twice", a.Name)
		case !def.ArtifactAllowed(a.Name):
			return store.CheckResult{}, errf(CodeInvalid, "artifact %q does not match the outputs of %s (artifacts = %s)", a.Name, def.Describe(), quoteList(def.Outputs.Artifacts))
		case int64(len(a.Data)) > max:
			return store.CheckResult{}, errf(CodeInvalid, "artifact %s is %s; one may be at most %s on this server", a.Name, FormatBytes(int64(len(a.Data))), FormatBytes(max))
		}
		names[a.Name] = true
		total += int64(len(a.Data))
	}
	if total > s.Checks.reportMax() {
		return store.CheckResult{}, errf(CodeInvalid, "this report is %s; one may carry at most %s on this server: attach fewer or smaller artifacts", FormatBytes(total), FormatBytes(s.Checks.reportMax()))
	}
	var data json.RawMessage
	if len(in.Data) > 0 {
		if int64(len(in.Data)) > max {
			return store.CheckResult{}, errf(CodeInvalid, "the data is %s; it may be at most %s", FormatBytes(int64(len(in.Data))), FormatBytes(max))
		}
		var v any
		if err := json.Unmarshal(in.Data, &v); err != nil {
			return store.CheckResult{}, errf(CodeInvalid, "the data%s is not JSON: %v", nameSuffix(in.DataName), err)
		}
		data = json.RawMessage(in.Data)
	}
	if in.Status == store.CheckPass {
		if missing := missingOutputs(def, names, in.DataName, data); len(missing) > 0 {
			return store.CheckResult{}, errf(CodeInvalid, "a passing result of %s must include %s (outputs.required): attach it with --artifact FILE or --data FILE", def.Describe(), strings.Join(missing, ", "))
		}
	}
	if !in.Started.IsZero() && !in.Finished.IsZero() && in.Finished.Before(in.Started) {
		return store.CheckResult{}, errf(CodeInvalid, "finished (%s) is before started (%s)", in.Finished.Format(time.RFC3339), in.Started.Format(time.RFC3339))
	}
	if total > 0 {
		if err := s.checkPushQuota(ctx, repo, total); err != nil {
			return store.CheckResult{}, err
		}
	}
	w := s.repoWrites(repo.ID)
	r := store.CheckResult{
		RepoID: repo.ID, EffortID: &eff.ID, ChangeID: t.ChangeID, Tree: t.Tree, CheckName: def.Name,
		Status: in.Status, Summary: clip(strings.TrimSpace(in.Summary), 4000), LogSize: int64(len(in.Log)),
		Data: data, DataName: in.DataName, ReporterID: &actor.ID, Reporter: actor.Ref(), OnBehalfOf: actor.OnBehalfOf(),
		Model: in.Model, CITokenID: in.CITokenID, How: strings.TrimSpace(in.How), Source: store.CheckSourceReport,
	}
	switch {
	case in.fromLand:
		r.Source = store.CheckSourceLand
	case in.isReview:
		r.Source = store.CheckSourceReview
	}
	if !in.Started.IsZero() {
		st := in.Started.UTC()
		r.StartedAt = &st
	}
	if !in.Finished.IsZero() {
		fi := in.Finished.UTC()
		r.FinishedAt = &fi
	}
	if len(in.Log) > 0 {
		h, err := w.Put(ctx, hash.KindBlob, in.Log)
		if err != nil {
			return store.CheckResult{}, err
		}
		r.LogHash = h
	}
	for _, a := range in.Artifacts {
		h, err := w.Put(ctx, hash.KindBlob, a.Data)
		if err != nil {
			return store.CheckResult{}, err
		}
		r.Artifacts = append(r.Artifacts, store.CheckArtifact{Name: a.Name, ContentType: contentType(a.Name, a.ContentType, a.Data), Hash: h, Size: int64(len(a.Data))})
	}
	var out store.CheckResult
	err := s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		if out, err = tx.InsertCheckResult(ctx, r); err != nil {
			return err
		}
		return tx.LogOp(ctx, &repo.ID, &actor.ID, "check-report", eff.Slug, nil, map[string]any{
			"result": out.ID.String(), "check": def.Name, "status": r.Status, "change": r.ChangeID, "tree": r.Tree.String(),
			"artifacts": len(r.Artifacts), "source": r.Source,
		})
	})
	return out, classify(err)
}

func nameSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

func quoteList(xs []string) string {
	qs := make([]string, len(xs))
	for i, x := range xs {
		qs[i] = fmt.Sprintf("%q", x)
	}
	return "[" + strings.Join(qs, ", ") + "]"
}

// missingOutputs are the outputs.required names a report lacks: each must
// be an artifact name, the data's file name or a top-level key of the
// data.
func missingOutputs(def rules.CheckDef, artifacts map[string]bool, dataName string, data json.RawMessage) []string {
	var keys map[string]json.RawMessage
	if len(data) > 0 {
		_ = json.Unmarshal(data, &keys)
	}
	var out []string
	for _, req := range def.Outputs.Required {
		if artifacts[req] || (dataName != "" && req == dataName) {
			continue
		}
		if _, ok := keys[req]; ok {
			continue
		}
		out = append(out, req)
	}
	return out
}

// contentType is the artifact's type: as given, else from its name, else
// sniffed from its bytes.
func contentType(name, given string, data []byte) string {
	if given = strings.TrimSpace(given); given != "" {
		return given
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(data)
}

// CheckState is one check of a change: whether it applies, its state on
// the change's tree, and its results there.
type CheckState struct {
	Def     rules.CheckDef
	Applies bool
	// State is CheckStateMissing, ..., or CheckStateNotNeeded when the
	// check does not apply.
	State string
	// Latest is the newest result that counts (its reporter is allowed).
	Latest *CheckResult
	// Results are the check's results on the tree, newest first (at most
	// maxStateResults), each saying whether it counts and why not.
	Results []CheckResult
	// How says how to provide a result, for people and agents.
	How string
}

// maxStateResults bounds CheckState.Results.
const maxStateResults = 10

// NotCounted are the results newer than Latest that do not count.
func (c CheckState) NotCounted() []CheckResult {
	var out []CheckResult
	for _, r := range c.Results {
		if c.Latest != nil && r.ID == c.Latest.ID {
			break
		}
		if !r.Counts {
			out = append(out, r)
		}
	}
	return out
}

// Blocks reports whether the check stops stage (land or ship) while it
// has no passing result.
func (c CheckState) Blocks(stage rules.Blocks) bool {
	return c.Applies && c.Def.Blocks == stage && c.State != CheckStatePassed
}

// CheckStatusInput selects a change for CheckStatus; see ReportInput.
type CheckStatusInput struct {
	Repo     string
	Effort   string
	ChangeID string
	Tree     hash.Hash
}

// CheckStatusView is what a change still needs.
type CheckStatusView struct {
	Repo     string
	Effort   string
	ChangeID string
	Tree     hash.Hash
	Head     bool // the effort head
	Landed   bool
	Checks   []CheckState
	// Other are results on this tree for no check of vx.toml, such as
	// vx land --run commands no check names.
	Other []store.CheckResult
	// Rules says where the checks came from; Problem is set when vx.toml
	// does not parse; Warnings say what of the checks was left out.
	Rules    string
	Problem  string
	Warnings []string
}

// CheckStatus says, for each check of a change, whether it applies and
// whether it is missing, pending, passed or failed on the change's tree
// (read).
func (s *Service) CheckStatus(ctx context.Context, actor Actor, in CheckStatusInput) (CheckStatusView, error) {
	repo, err := s.authorizeCheckRead(ctx, actor, in.Repo)
	if err != nil {
		return CheckStatusView{}, err
	}
	eff, err := s.Store.GetEffort(ctx, repo.ID, in.Effort)
	if err != nil {
		return CheckStatusView{}, classify(err)
	}
	return s.checkStatusOf(ctx, repo, eff, in.ChangeID, in.Tree)
}

// checkStatusOf is CheckStatus without an access check, for the server's
// own work (pipelines, GitHub check runs) after the caller was
// authorized.
func (s *Service) checkStatusOf(ctx context.Context, repo store.Repo, eff store.Effort, changeID string, tree hash.Hash) (CheckStatusView, error) {
	t, err := s.resolveCheckTarget(ctx, repo, eff, changeID, tree)
	if err != nil {
		return CheckStatusView{}, err
	}
	return s.checkStatus(ctx, t, nil)
}

// checkStatus computes the view for t; ran are the vx land --run results
// of a land in progress, which count for run checks of their command.
func (s *Service) checkStatus(ctx context.Context, t checkTarget, ran []object.Check) (CheckStatusView, error) {
	v := CheckStatusView{Repo: t.Repo.Name, Effort: t.Effort.Slug, ChangeID: t.ChangeID, Tree: t.Tree, Head: t.Head, Landed: t.Landed}
	head, err := s.Store.GetChangeset(ctx, t.Repo.ID, t.Effort.Head)
	if err != nil {
		return v, classify(err)
	}
	defs, r, warnings := s.checkDefs(ctx, t.Repo, t.Tree, head.Tree)
	v.Rules = r.Source
	v.Warnings = warnings
	if _, err := rules.At(ctx, s.Blobs, t.Tree); err != nil {
		v.Problem = err.Error()
	}
	changePaths, err := s.changedPaths(ctx, t.BaseTree, t.Tree)
	if err != nil {
		return v, err
	}
	effortPaths := changePaths
	if t.Head && !t.EffortBaseTree.IsZero() {
		if effortPaths, err = s.changedPaths(ctx, t.EffortBaseTree, t.Tree); err != nil {
			return v, err
		}
	}
	results, err := s.Store.ListCheckResults(ctx, store.CheckResultFilter{RepoID: t.Repo.ID, Tree: t.Tree})
	if err != nil {
		return v, err
	}
	used := map[uuid.UUID]bool{}
	for _, d := range defs {
		paths := changePaths
		if d.Blocks == rules.BlocksShip || d.When == rules.WhenShip {
			paths = effortPaths
		}
		st := CheckState{Def: d, Applies: d.Applies(paths), How: howToProvide(d), State: CheckStateNotNeeded}
		if st.Applies {
			st.State = CheckStateMissing
		}
		for _, c := range ran {
			if d.IsRun() && c.RanByVX && strings.TrimSpace(c.Name) == d.Run {
				res := CheckResult{CheckResult: store.CheckResult{CheckName: d.Name, How: d.Run, Status: c.Result, Source: store.CheckSourceLand, Reporter: "this land"}, Counts: true}
				st.Latest = &res
				break
			}
		}
		for _, res := range results {
			if !resultFor(d, res) {
				continue
			}
			used[res.ID] = true
			cr := CheckResult{CheckResult: res, RepoName: t.Repo.Name, EffortSlug: t.Effort.Slug}
			cr.Counts, cr.Why = s.resultCounts(ctx, d, t.Author, res)
			if cr.Counts && st.Latest == nil {
				latest := cr
				st.Latest = &latest
			}
			if len(st.Results) < maxStateResults {
				st.Results = append(st.Results, cr)
			}
		}
		if st.Applies && st.Latest != nil {
			st.State = stateOf(st.Latest.Status)
		}
		v.Checks = append(v.Checks, st)
	}
	for _, res := range results {
		if !used[res.ID] {
			v.Other = append(v.Other, res)
		}
	}
	return v, nil
}

// resultFor reports whether a stored result is one of check d: named so,
// or, for a run check, an unnamed vx land --run result of its command.
func resultFor(d rules.CheckDef, r store.CheckResult) bool {
	if r.CheckName == d.Name && r.CheckName != "" {
		return true
	}
	return d.IsRun() && r.CheckName == "" && strings.TrimSpace(r.How) == d.Run
}

func stateOf(status string) string {
	switch status {
	case store.CheckPass:
		return CheckStatePassed
	case store.CheckFail:
		return CheckStateFailed
	case store.CheckNeutral:
		return CheckStateNeutral
	case store.CheckPending:
		return CheckStatePending
	}
	return CheckStateMissing
}

// howToProvide says how a result of d is provided.
func howToProvide(d rules.CheckDef) string {
	switch {
	case d.Origin == rules.OriginPolicy && d.Name == rules.PolicyReviewCheck:
		return "land, then a person approves it: vx review approve ID"
	case d.IsRun():
		return "vx land --run " + rules.ShellQuote(d.Run) + " (or vx check run " + d.Name + ")"
	}
	who := map[rules.Reporter]string{
		rules.ReporterSelf:        "",
		rules.ReporterIndependent: " (by someone independent: not the author, nor another agent of the author's person)",
		rules.ReporterCI:          " (by CI, with a CI service token)",
		rules.ReporterHuman:       " (by a person)",
	}[d.Reporter]
	runner := ""
	if d.Runner != "" {
		runner = "runner " + d.Runner + ": "
	}
	return runner + "vx check report " + d.Name + " --status pass --summary TEXT" + who
}

// landGate refuses a land whose snapshot lacks a passing result for a
// check of vx.toml ([[check]] or an organization rule) that blocks land
// and applies to the files it changes. The checks of [checks] required
// and [[policy]] stay with landPolicy. It reports whether any such check
// applies: the land must then land exactly the snapshot, never a merge.
func (s *Service) landGate(ctx context.Context, actor Actor, v WorkspaceView, base store.ChangesetRow, in LandInput) (bool, error) {
	t := checkTarget{Repo: v.Repo, Effort: v.Effort, ChangeID: v.Workspace.ChangeID, Tree: in.Tree, BaseTree: base.Tree,
		Author: authorOf(actor.Ref(), actor.OnBehalfOf())}
	if !s.hasLandGates(ctx, v.Repo, in.Tree, v.Effort.Head) {
		return false, nil
	}
	var ran []object.Check
	if in.Evidence != nil {
		ran = in.Evidence.Checks
	}
	view, err := s.checkStatus(ctx, t, ran)
	if err != nil {
		return false, err
	}
	var missing []CheckState
	gated := false
	for _, c := range view.Checks {
		if c.Def.Origin == rules.OriginRequired || c.Def.Origin == rules.OriginPolicy {
			continue
		}
		if c.Applies && c.Def.Blocks == rules.BlocksLand {
			gated = true
			if c.State != CheckStatePassed {
				missing = append(missing, c)
			}
		}
	}
	if len(missing) == 0 {
		return gated, nil
	}
	msg := gateRefusal("land", fmt.Sprintf("this change's tree %s (change %s)", in.Tree.Short(), v.Workspace.ChangeID), missing)
	// Checks with a runner become jobs on this snapshot, so runners make
	// the results the next land needs.
	var defs []jobDef
	var names []string
	for _, c := range missing {
		if c.Def.Runner != "" {
			defs, names = append(defs, jobDefOf(c.Def)), append(names, c.Def.Name)
		}
	}
	if len(defs) > 0 {
		change := JobChangeRef{ID: v.Workspace.ChangeID, Intent: v.Workspace.Intent, Author: t.Author}
		jobs, err := s.enqueueGateJobs(ctx, v.Repo, v.Effort, change, base.Tree, in.Tree, defs, names)
		if err != nil {
			slog.Warn("check jobs for a land gate", "repo", v.Repo.Name, "effort", v.Effort.Slug, "err", err)
		}
		for _, j := range jobs {
			msg += fmt.Sprintf("\nQueued %s for runner %s as job %s (vx check jobs).", j.Check, j.Runner, shortID(j.ID))
		}
	}
	return gated, &Error{Code: CodeFailedPrecondition, Msg: msg}
}

// hasLandGates reports whether the snapshot's or the effort head's
// vx.toml has a check of its own (not [checks] required or [[policy]])
// that blocks land, so lands of repositories without them cost nothing.
func (s *Service) hasLandGates(ctx context.Context, repo store.Repo, tree, effortHead hash.Hash) bool {
	trees := []hash.Hash{tree}
	if head, err := s.Store.GetChangeset(ctx, repo.ID, effortHead); err == nil {
		trees = append(trees, head.Tree)
	}
	defs, _, _ := s.checkDefs(ctx, repo, trees...)
	for _, d := range defs {
		if d.Blocks == rules.BlocksLand && d.Origin != rules.OriginRequired && d.Origin != rules.OriginPolicy {
			return true
		}
	}
	return false
}

// gateRefusal says which checks stop a land or a ship and how to provide
// them:
//
//	not landed: 2 checks that block land have no passing result on this change's tree 3f2a1c9d0e7b (change kxqr-7m2p):
//	  unit             missing   vx land --run 'go test ./...' (or vx check run unit)
//	  security-review  failed    agent:bob/sec at 14:02: "SQL built from input in x.go"
//	Provide them, then land again; vx check status shows what the change still needs.
func gateRefusal(stage, subject string, missing []CheckState) string {
	var b strings.Builder
	verb := map[string]string{"land": "not landed", "ship": "not shipped"}[stage]
	n := fmt.Sprintf("%d checks that block %s have", len(missing), stage)
	if len(missing) == 1 {
		n = fmt.Sprintf("1 check that blocks %s has", stage)
	}
	fmt.Fprintf(&b, "%s: %s no passing result on %s:\n", verb, n, subject)
	width := 0
	for _, c := range missing {
		if len(c.Def.Name) > width {
			width = len(c.Def.Name)
		}
	}
	for _, c := range missing {
		detail := c.How
		if c.Latest != nil && c.State != CheckStateMissing {
			detail = fmt.Sprintf("%s at %s", c.Latest.Reporter, c.Latest.CreatedAt.UTC().Format("2006-01-02 15:04"))
			if c.Latest.Summary != "" {
				detail += fmt.Sprintf(": %q", clip(c.Latest.Summary, 120))
			}
			detail += "; then " + c.How
		} else if nc := c.NotCounted(); len(nc) > 0 {
			detail = "a result does not count (" + nc[0].Why + "); " + c.How
		}
		fmt.Fprintf(&b, "  %-*s  %-8s %s\n", width, c.Def.Name, c.State, detail)
	}
	fmt.Fprintf(&b, "Provide them, then %s again; vx check status shows what the change still needs.", stage)
	return b.String()
}

// recordLandChecks stores the vx land --run results of a land as check
// results of the workspace's change on its snapshot (the tree they ran
// on): under the name of the [[check]] that runs the command, else
// unnamed with the command as how. Reported --check results are kept in
// the changeset's evidence only.
func (s *Service) recordLandChecks(ctx context.Context, tx *store.Tx, actor Actor, v WorkspaceView, in LandInput) ([]store.CheckResult, error) {
	if in.Evidence == nil {
		return nil, nil
	}
	var out []store.CheckResult
	r, _ := rules.At(ctx, s.Blobs, in.Tree)
	now := s.Clock().UTC()
	for _, c := range in.Evidence.Checks {
		if !c.RanByVX {
			continue
		}
		status := store.CheckFail
		if c.Result == "pass" {
			status = store.CheckPass
		}
		name := ""
		if d, ok := r.RunCheck(c.Name); ok {
			name = d.Name
		}
		started := now.Add(-time.Duration(c.DurationMs) * time.Millisecond)
		res := store.CheckResult{
			RepoID: v.Repo.ID, EffortID: &v.Effort.ID, ChangeID: v.Workspace.ChangeID, Tree: in.Tree, CheckName: name,
			Status: status, Summary: c.Notes, ReporterID: &actor.ID, Reporter: actor.Ref(), OnBehalfOf: actor.OnBehalfOf(),
			Model: in.Model, How: strings.TrimSpace(c.Name), Source: store.CheckSourceLand, StartedAt: &started, FinishedAt: &now,
		}
		stored, err := tx.InsertCheckResult(ctx, res)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

// afterLandChecks runs afterCheckResult for the --run results a land
// stored under a check's name (they count: vx ran the command of the
// check, and run checks accept any reporter rule but their command).
func (s *Service) afterLandChecks(ctx context.Context, repo store.Repo, eff store.Effort, res LandResult) {
	if len(res.CheckResults) == 0 {
		return
	}
	defs, _, _ := s.checkDefs(ctx, repo, res.CheckResults[0].Tree)
	for _, r := range res.CheckResults {
		if r.CheckName == "" {
			continue
		}
		if d, ok := findCheck(defs, r.CheckName); ok && r.Status == store.CheckPass {
			if counts, _ := s.resultCounts(ctx, d, authorOf(r.Reporter, r.OnBehalfOf), r); counts {
				s.afterCheckResult(ctx, repo, eff.ID, d.Name, r.Tree, r.ID)
			}
		}
	}
}

// recordReviewCheck stores a person's approval under a human [[policy]]
// as a passing result of the policy's check "review" on the reviewed
// snapshot.
func (s *Service) recordReviewCheck(ctx context.Context, tx *store.Tx, actor Actor, r store.Review) error {
	now := s.Clock().UTC()
	_, err := tx.InsertCheckResult(ctx, store.CheckResult{
		RepoID: r.RepoID, EffortID: &r.EffortID, ChangeID: r.ChangeID, Tree: r.Tree, CheckName: rules.PolicyReviewCheck,
		Status: store.CheckPass, Summary: "approved review " + shortID(r.ID), ReporterID: &actor.ID, Reporter: actor.Ref(),
		OnBehalfOf: actor.OnBehalfOf(), Source: store.CheckSourceReview, FinishedAt: &now,
	})
	return err
}

// shipGate refuses to ship changes that touch paths while a check of the
// effort head's vx.toml that blocks ship has no passing result on the
// effort head's tree. what names the shipment for the refusal ("the
// effort", "part 2 of 3").
func (s *Service) shipGate(ctx context.Context, repo store.Repo, eff store.Effort, paths []string, what string) error {
	head, err := s.Store.GetChangeset(ctx, repo.ID, eff.Head)
	if err != nil {
		return classify(err)
	}
	t := checkTarget{Repo: repo, Effort: eff, ChangeID: head.ChangeID, Tree: head.Tree, Head: true, Landed: true,
		Author: authorOf(head.Author.Principal, head.Author.OnBehalfOf)}
	if len(head.Parents) > 0 {
		if p, err := s.Store.GetChangeset(ctx, repo.ID, head.Parents[0]); err == nil {
			t.BaseTree = p.Tree
		}
	}
	view, err := s.checkStatus(ctx, t, nil)
	if err != nil {
		return err
	}
	var missing []CheckState
	for _, c := range view.Checks {
		if c.Def.Blocks != rules.BlocksShip || !c.Def.Applies(paths) {
			continue
		}
		if c.Latest == nil || c.Latest.Status != store.CheckPass {
			if c.State == CheckStateNotNeeded {
				c.State = CheckStateMissing
				if c.Latest != nil {
					c.State = stateOf(c.Latest.Status)
				}
			}
			c.Applies = true
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return errf(CodeFailedPrecondition, "%s", gateRefusal("ship", fmt.Sprintf("the effort head's tree %s (change %s), which %s ships", head.Tree.Short(), head.ChangeID, what), missing))
}

// GetCheckResult returns one result (read).
func (s *Service) GetCheckResult(ctx context.Context, actor Actor, id string) (CheckResult, error) {
	r, err := s.checkResult(ctx, actor, id)
	if err != nil {
		return CheckResult{}, err
	}
	repo, err := s.Store.RepoByID(ctx, r.RepoID)
	if err != nil {
		return CheckResult{}, classify(err)
	}
	out := CheckResult{CheckResult: r, RepoName: repo.Name}
	if r.EffortID != nil {
		if eff, err := s.Store.EffortByID(ctx, *r.EffortID); err == nil {
			out.EffortSlug = eff.Slug
		}
	}
	return out, nil
}

// checkResult resolves an id (or a prefix of 8 or more characters) among
// the results the caller can read; others are not found.
func (s *Service) checkResult(ctx context.Context, actor Actor, id string) (store.CheckResult, error) {
	id = strings.TrimSpace(id)
	notFound := errf(CodeNotFound, "no check result %s; vx check status lists the results of a change", id)
	if len(id) < 8 {
		return store.CheckResult{}, errf(CodeInvalid, "give at least 8 characters of the check result id")
	}
	ids, all, err := s.ReadableRepoIDs(ctx, actor)
	if err != nil {
		return store.CheckResult{}, err
	}
	var repos []uuid.UUID
	if !all {
		if len(ids) == 0 {
			return store.CheckResult{}, notFound
		}
		repos = ids
	}
	r, err := s.Store.CheckResultByPrefix(ctx, id, repos)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.CheckResult{}, notFound
		}
		return store.CheckResult{}, classify(err)
	}
	if _, err := s.AuthorizeID(ctx, actor, r.RepoID, RoleRead); err != nil {
		if isHidden(err) {
			return store.CheckResult{}, notFound
		}
		return store.CheckResult{}, err
	}
	return r, nil
}

// CheckArtifact returns one artifact of a result, or its log when name is
// empty (read).
func (s *Service) CheckArtifact(ctx context.Context, actor Actor, id, name string) (store.CheckArtifact, []byte, error) {
	r, err := s.checkResult(ctx, actor, id)
	if err != nil {
		return store.CheckArtifact{}, nil, err
	}
	var a store.CheckArtifact
	if name == "" {
		if r.LogHash.IsZero() {
			return a, nil, errf(CodeNotFound, "check result %s has no log", shortID(r.ID))
		}
		a = store.CheckArtifact{Name: "log.txt", ContentType: "text/plain; charset=utf-8", Hash: r.LogHash, Size: r.LogSize}
	} else {
		found := false
		for _, x := range r.Artifacts {
			if x.Name == name {
				a, found = x, true
				break
			}
		}
		if !found {
			var names []string
			for _, x := range r.Artifacts {
				names = append(names, x.Name)
			}
			sort.Strings(names)
			return a, nil, errf(CodeNotFound, "check result %s has no artifact %q (it has: %s)", shortID(r.ID), name, strings.Join(names, ", "))
		}
	}
	data, err := blob.GetKind(ctx, s.Blobs, hash.KindBlob, a.Hash)
	if err != nil {
		return a, nil, classify(err)
	}
	return a, data, nil
}

// CheckResults lists the results bound to a change's tree (read), newest
// first: what pipelines read to feed one check's outputs into the next.
func (s *Service) CheckResults(ctx context.Context, actor Actor, repoName, effort, changeID string, tree hash.Hash) ([]store.CheckResult, error) {
	repo, err := s.authorizeCheckRead(ctx, actor, repoName)
	if err != nil {
		return nil, err
	}
	eff, err := s.Store.GetEffort(ctx, repo.ID, effort)
	if err != nil {
		return nil, classify(err)
	}
	t, err := s.resolveCheckTarget(ctx, repo, eff, changeID, tree)
	if err != nil {
		return nil, err
	}
	return s.Store.ListCheckResults(ctx, store.CheckResultFilter{RepoID: repo.ID, Tree: t.Tree})
}
