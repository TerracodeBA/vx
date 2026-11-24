package mcp

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/merge"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// Limits on what one tool call returns.
const (
	defaultLogLimit = 20
	maxLogLimit     = 200
	statusLands     = 5
	maxDiffBytes    = 64 << 10 // diff text returned by vx_diff
	maxDiffInput    = 4 << 20  // file contents vx_diff is willing to diff
	maxChainCount   = 10000    // changes counted per effort by vx_efforts
)

// ---------- outputs ----------

// Principal is who someone is.
type Principal struct {
	Ref         string `json:"ref" jsonschema:"principal reference: user:<handle> or agent:<person>/<name>"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name,omitempty"`
	OnBehalfOf  string `json:"on_behalf_of,omitempty" jsonschema:"for agents: the person the agent acts for"`
}

// Effort is one effort in a listing.
type Effort struct {
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	State          string `json:"state" jsonschema:"open, shipping, merged or abandoned"`
	BaseBranch     string `json:"base_branch"`
	Head           string `json:"head"`
	Changes        int    `json:"changes" jsonschema:"changes landed since the effort's base"`
	OpenWorkspaces int    `json:"open_workspaces"`
	Owner          string `json:"owner,omitempty"`
}

// Repo is a repository and its efforts.
type Repo struct {
	Name          string   `json:"name"`
	DefaultBranch string   `json:"default_branch"`
	Remote        string   `json:"remote,omitempty"`
	Efforts       []Effort `json:"efforts"`
}

// Workspace is an open task in an effort.
type Workspace struct {
	Owner    string `json:"owner"`
	Intent   string `json:"intent"`
	ChangeID string `json:"change_id"`
	Since    string `json:"since"`
}

// Author is who landed a change.
type Author struct {
	Principal  string `json:"principal"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	Model      string `json:"model,omitempty"`
}

// Check is one piece of evidence.
type Check struct {
	Name    string `json:"name"`
	Result  string `json:"result" jsonschema:"pass or fail"`
	RanByVX bool   `json:"ran_by_vx" jsonschema:"true: vx ran the command (vx land --run); false: the author reported it"`
	Passed  int    `json:"passed,omitempty"`
	Failed  int    `json:"failed,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

// Evidence is what the author offered as proof the change works.
type Evidence struct {
	Summary  string   `json:"summary" jsonschema:"one line, e.g. '3 checks passed (3 run by vx), 2 new tests'"`
	Checks   []Check  `json:"checks,omitempty"`
	NewTests []string `json:"new_tests,omitempty"`
	// BeforeMerge: the server merged the author's snapshot onto newer work.
	BeforeMerge bool `json:"before_merge,omitempty" jsonschema:"the server merged this change onto newer work, so the evidence was gathered on the author's snapshot, not the landed tree"`
}

// Warning is a problem the server found when the change landed.
type Warning struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// File is one file a change touched.
type File struct {
	Path    string `json:"path"`
	Op      string `json:"op" jsonschema:"add, modify or delete"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// Change is a landed change.
type Change struct {
	Hash      string    `json:"hash"`
	ChangeID  string    `json:"change_id"`
	Intent    string    `json:"intent"`
	Author    Author    `json:"author"`
	CreatedAt string    `json:"created_at"`
	Evidence  Evidence  `json:"evidence"`
	Warnings  []Warning `json:"warnings,omitempty"`
	Files     []File    `json:"files,omitempty"`
}

// ChangeDetail is one change with everything vx knows about it.
type ChangeDetail struct {
	Change
	Effort    string   `json:"effort,omitempty"`
	Origin    string   `json:"origin" jsonschema:"vx: landed in vx; upstream: imported from git"`
	GitSHA    string   `json:"git_sha,omitempty"`
	Parents   []string `json:"parents,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// ---------- inputs ----------

type noInput struct{}

type effortsInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"repository name, e.g. acme/shop-api; empty: every repository"`
}

type effortInput struct {
	Repo   string `json:"repo" jsonschema:"repository name, e.g. acme/shop-api"`
	Effort string `json:"effort" jsonschema:"effort slug, as vx efforts lists it"`
}

type logInput struct {
	Repo   string `json:"repo" jsonschema:"repository name"`
	Effort string `json:"effort" jsonschema:"effort slug"`
	Limit  int    `json:"limit,omitempty" jsonschema:"number of changes, newest first (default 20, at most 200)"`
	Files  bool   `json:"files,omitempty" jsonschema:"include the files each change touched"`
}

type showInput struct {
	Repo string `json:"repo" jsonschema:"repository name"`
	Ref  string `json:"ref" jsonschema:"change id (e.g. qx9b-mwns), change id prefix, hash or hash prefix"`
}

type diffInput struct {
	Repo string `json:"repo" jsonschema:"repository name"`
	Ref  string `json:"ref" jsonschema:"change id, change id prefix, hash or hash prefix"`
	Path string `json:"path" jsonschema:"path of one file the change touched, as vx_show lists it"`
}

// ---------- results ----------

type effortsOutput struct {
	Repos []Repo `json:"repos"`
}

type statusOutput struct {
	Repo           string      `json:"repo"`
	Effort         string      `json:"effort"`
	Name           string      `json:"name"`
	Goal           string      `json:"goal,omitempty"`
	State          string      `json:"state"`
	BaseBranch     string      `json:"base_branch"`
	Base           string      `json:"base"`
	Head           string      `json:"head"`
	OpenWorkspaces []Workspace `json:"open_workspaces"`
	RecentLands    []Change    `json:"recent_lands" jsonschema:"the last 5 changes landed, newest first"`
	Warnings       []string    `json:"warnings,omitempty" jsonschema:"warnings stored with the recent lands, each naming its change"`
}

type logOutput struct {
	Changes []Change `json:"changes"`
}

type diffOutput struct {
	Path      string `json:"path"`
	Op        string `json:"op"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"the diff was cut at 64 KiB or the file is too large to diff"`
	Diff      string `json:"diff" jsonschema:"unified diff with 3 lines of context"`
}

// ---------- registration ----------

func readOnly(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

func (t *tools) register(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "vx_whoami",
		Description: "Who the vx token belongs to: your principal and, for an agent, the person it acts for.",
		Annotations: readOnly("Who am I"),
	}, t.whoami)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "vx_efforts",
		Description: "List repositories and their efforts with state, number of changes landed and open workspaces.",
		Annotations: readOnly("List efforts"),
	}, t.efforts)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "vx_status",
		Description: "Status of one effort: head, base branch, open workspaces (who is working on what), " +
			"the last 5 lands with authors and evidence, and warnings found in them.",
		Annotations: readOnly("Effort status"),
	}, t.status)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "vx_log",
		Description: "Changes landed in an effort since its base, newest first, with authors and evidence; optionally the files each touched.",
		Annotations: readOnly("Effort log"),
	}, t.logTool)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "vx_show",
		Description: "One change: intent, author, files with line counts, evidence (checks and new tests) and warnings.",
		Annotations: readOnly("Show change"),
	}, t.show)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "vx_diff",
		Description: "Unified diff of one file in one change (3 lines of context, cut at 64 KiB). Use vx_show first to list the files.",
		Annotations: readOnly("Diff one file"),
	}, t.diff)
}

// ---------- handlers ----------

func (t *tools) whoami(ctx context.Context, req *sdk.CallToolRequest, _ noInput) (*sdk.CallToolResult, Principal, error) {
	a, err := actorOf(req)
	if err != nil {
		return nil, Principal{}, err
	}
	return nil, Principal{Ref: a.Ref(), Kind: string(a.Kind), DisplayName: a.DisplayName, OnBehalfOf: a.OnBehalfOf()}, nil
}

func (t *tools) efforts(ctx context.Context, req *sdk.CallToolRequest, in effortsInput) (*sdk.CallToolResult, effortsOutput, error) {
	if _, err := actorOf(req); err != nil {
		return nil, effortsOutput{}, err
	}
	var repos []store.Repo
	if in.Repo != "" {
		r, err := t.svc.Store.RepoByName(ctx, in.Repo)
		if err != nil {
			return nil, effortsOutput{}, t.userError("vx_efforts", err)
		}
		repos = []store.Repo{r}
	} else {
		var err error
		if repos, err = t.svc.Store.ListRepos(ctx); err != nil {
			return nil, effortsOutput{}, t.userError("vx_efforts", err)
		}
	}
	refs := newRefs(t.svc)
	out := effortsOutput{Repos: []Repo{}}
	for _, r := range repos {
		_, effs, err := t.svc.ListEfforts(ctx, r.Name, "")
		if err != nil {
			return nil, effortsOutput{}, t.userError("vx_efforts", err)
		}
		repo := Repo{Name: r.Name, DefaultBranch: r.DefaultBranch, Remote: r.Remote, Efforts: []Effort{}}
		for _, e := range effs {
			open, err := t.svc.Store.ListWorkspaces(ctx, e.ID, store.WorkspaceOpen)
			if err != nil {
				return nil, effortsOutput{}, t.userError("vx_efforts", err)
			}
			n, err := t.svc.Store.ChainLength(ctx, r.ID, e.Head, e.Base, maxChainCount)
			if err != nil {
				return nil, effortsOutput{}, t.userError("vx_efforts", err)
			}
			repo.Efforts = append(repo.Efforts, Effort{
				Slug: e.Slug, Name: e.Name, State: string(e.State), BaseBranch: e.BaseBranch,
				Head: e.Head.String(), Changes: n, OpenWorkspaces: len(open), Owner: refs.of(ctx, e.OwnerID),
			})
		}
		out.Repos = append(out.Repos, repo)
	}
	return nil, out, nil
}

func (t *tools) status(ctx context.Context, req *sdk.CallToolRequest, in effortInput) (*sdk.CallToolResult, statusOutput, error) {
	if _, err := actorOf(req); err != nil {
		return nil, statusOutput{}, err
	}
	if err := need(in.Repo, "repo", in.Effort, "effort"); err != nil {
		return nil, statusOutput{}, err
	}
	v, err := t.svc.GetEffort(ctx, in.Repo, in.Effort)
	if err != nil {
		return nil, statusOutput{}, t.userError("vx_status", err)
	}
	entries, err := t.svc.Log(ctx, in.Repo, in.Effort, statusLands, false)
	if err != nil {
		return nil, statusOutput{}, t.userError("vx_status", err)
	}
	refs := newRefs(t.svc)
	out := statusOutput{
		Repo: v.Repo.Name, Effort: v.Effort.Slug, Name: v.Effort.Name, Goal: v.Effort.Goal,
		State: string(v.Effort.State), BaseBranch: v.Effort.BaseBranch,
		Base: v.Effort.Base.String(), Head: v.Effort.Head.String(),
		OpenWorkspaces: workspaces(ctx, refs, v.Workspaces), RecentLands: changes(entries, false),
	}
	for _, e := range entries {
		for _, w := range e.Warnings {
			out.Warnings = append(out.Warnings, w.Detail+" (change "+e.ChangeID+")")
		}
	}
	return nil, out, nil
}

func (t *tools) logTool(ctx context.Context, req *sdk.CallToolRequest, in logInput) (*sdk.CallToolResult, logOutput, error) {
	if _, err := actorOf(req); err != nil {
		return nil, logOutput{}, err
	}
	if err := need(in.Repo, "repo", in.Effort, "effort"); err != nil {
		return nil, logOutput{}, err
	}
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultLogLimit
	case limit > maxLogLimit:
		limit = maxLogLimit
	}
	entries, err := t.svc.Log(ctx, in.Repo, in.Effort, limit, in.Files)
	if err != nil {
		return nil, logOutput{}, t.userError("vx_log", err)
	}
	return nil, logOutput{Changes: changes(entries, in.Files)}, nil
}

func (t *tools) show(ctx context.Context, req *sdk.CallToolRequest, in showInput) (*sdk.CallToolResult, ChangeDetail, error) {
	if _, err := actorOf(req); err != nil {
		return nil, ChangeDetail{}, err
	}
	if err := need(in.Repo, "repo", in.Ref, "ref"); err != nil {
		return nil, ChangeDetail{}, err
	}
	_, e, err := t.svc.GetChangeset(ctx, in.Repo, strings.TrimSpace(in.Ref))
	if err != nil {
		return nil, ChangeDetail{}, t.userError("vx_show", err)
	}
	out := ChangeDetail{
		Change: change(e, true), Origin: string(e.Origin), GitSHA: e.GitSHA, DependsOn: e.DependsOn,
	}
	if out.Files == nil {
		out.Files = []File{}
	}
	for _, p := range e.Parents {
		out.Parents = append(out.Parents, p.String())
	}
	if e.EffortID != nil {
		if eff, err := t.svc.Store.EffortByID(ctx, *e.EffortID); err == nil {
			out.Effort = eff.Slug
		}
	}
	return nil, out, nil
}

func (t *tools) diff(ctx context.Context, req *sdk.CallToolRequest, in diffInput) (*sdk.CallToolResult, diffOutput, error) {
	if _, err := actorOf(req); err != nil {
		return nil, diffOutput{}, err
	}
	if err := need(in.Repo, "repo", in.Ref, "ref", in.Path, "path"); err != nil {
		return nil, diffOutput{}, err
	}
	_, e, err := t.svc.GetChangeset(ctx, in.Repo, strings.TrimSpace(in.Ref))
	if err != nil {
		return nil, diffOutput{}, t.userError("vx_diff", err)
	}
	path := strings.TrimPrefix(strings.TrimSpace(in.Path), "./")
	var fc *store.FileChange
	for i := range e.Files {
		if e.Files[i].Path == path {
			fc = &e.Files[i]
			break
		}
	}
	if fc == nil {
		return nil, diffOutput{}, invalid("change " + e.ChangeID + " does not touch " + path + "; vx_show lists the files it touched")
	}
	out := diffOutput{Path: fc.Path, Op: string(fc.Op)}
	a, err := t.readBlob(ctx, fc.Old.Hash)
	if err != nil {
		return nil, diffOutput{}, t.userError("vx_diff", err)
	}
	b, err := t.readBlob(ctx, fc.New.Hash)
	if err != nil {
		return nil, diffOutput{}, t.userError("vx_diff", err)
	}
	if merge.IsBinary(a) || merge.IsBinary(b) {
		out.Binary = true
	}
	if !out.Binary && len(a)+len(b) > maxDiffInput {
		out.Truncated = true
		out.Diff = "file too large to diff here; it changed by +" + itoa(fc.Added) + " -" + itoa(fc.Deleted) + " lines"
		return nil, out, nil
	}
	oldName, newName := fc.Path, fc.Path
	switch fc.Op {
	case object.OpAdd:
		oldName = ""
	case object.OpDelete:
		newName = ""
	}
	out.Diff, out.Truncated = capText(merge.Unified(oldName, newName, a, b, 3), maxDiffBytes)
	return nil, out, nil
}

func (t *tools) readBlob(ctx context.Context, h hash.Hash) ([]byte, error) {
	if h.IsZero() {
		return nil, nil
	}
	return blob.GetKind(ctx, t.svc.Blobs, hash.KindBlob, h)
}

// ---------- conversion ----------

func changes(entries []service.LogEntry, withFiles bool) []Change {
	out := make([]Change, 0, len(entries))
	for _, e := range entries {
		out = append(out, change(e, withFiles))
	}
	return out
}

func change(e service.LogEntry, withFiles bool) Change {
	c := Change{
		Hash: e.Hash.String(), ChangeID: e.ChangeID, Intent: e.Intent,
		Author:    Author{Principal: e.Author.Principal, OnBehalfOf: e.Author.OnBehalfOf, Model: e.Author.Model},
		CreatedAt: ts(e.CreatedAt), Evidence: evidence(e.Changeset),
	}
	for _, w := range e.Warnings {
		c.Warnings = append(c.Warnings, Warning{Path: w.Path, Kind: w.Kind, Detail: w.Detail})
	}
	if withFiles {
		c.Files = files(e.Files)
	}
	return c
}

func files(fs []store.FileChange) []File {
	out := make([]File, 0, len(fs))
	for _, f := range fs {
		out = append(out, File{Path: f.Path, Op: string(f.Op), Added: f.Added, Deleted: f.Deleted})
	}
	return out
}

func evidence(cs *object.Changeset) Evidence {
	out := Evidence{BeforeMerge: cs.Merged()}
	ev := cs.Evidence
	if ev == nil || ev.IsEmpty() {
		out.Summary = "no evidence"
		return out
	}
	passed, failed, byVX := 0, 0, 0
	var failing []string
	for _, c := range ev.Checks {
		out.Checks = append(out.Checks, Check{Name: c.Name, Result: c.Result, RanByVX: c.RanByVX, Passed: c.Passed, Failed: c.Failed, Notes: c.Notes})
		if c.RanByVX {
			byVX++
		}
		if c.Result == "pass" {
			passed++
		} else {
			failed++
			failing = append(failing, c.Name)
		}
	}
	out.NewTests = ev.NewTests
	var parts []string
	if len(ev.Checks) > 0 {
		s := plural(passed, "check") + " passed"
		if failed > 0 {
			s += ", " + itoa(failed) + " failed (" + strings.Join(failing, "; ") + ")"
		}
		s += " (" + itoa(byVX) + " run by vx)"
		parts = append(parts, s)
	}
	if len(ev.NewTests) > 0 {
		parts = append(parts, plural(len(ev.NewTests), "new test"))
	}
	out.Summary = strings.Join(parts, ", ")
	if out.BeforeMerge {
		out.Summary += ", gathered before the server merged it"
	}
	return out
}

func workspaces(ctx context.Context, refs *refs, ws []store.Workspace) []Workspace {
	out := make([]Workspace, 0, len(ws))
	for _, w := range ws {
		out = append(out, Workspace{Owner: refs.of(ctx, w.OwnerID), Intent: w.Intent, ChangeID: w.ChangeID, Since: ts(w.CreatedAt)})
	}
	return out
}

// refs resolves principal ids to refs, once per id per call.
type refs struct {
	svc   *service.Service
	cache map[uuid.UUID]string
}

func newRefs(svc *service.Service) *refs { return &refs{svc: svc, cache: map[uuid.UUID]string{}} }

func (r *refs) of(ctx context.Context, id uuid.UUID) string {
	if ref, ok := r.cache[id]; ok {
		return ref
	}
	ref := ""
	if p, err := r.svc.Store.PrincipalByID(ctx, id); err == nil {
		ref = p.Ref()
	}
	r.cache[id] = ref
	return ref
}

// ---------- helpers ----------

// need checks required string arguments, given as value, name pairs.
func need(pairs ...string) error {
	var missing []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.TrimSpace(pairs[i]) == "" {
			missing = append(missing, pairs[i+1])
		}
	}
	if len(missing) > 0 {
		return invalid("missing " + strings.Join(missing, ", ") + "; vx_efforts lists repositories and effort slugs")
	}
	return nil
}

// capText cuts s to at most n bytes at a line boundary.
func capText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := strings.LastIndexByte(s[:n], '\n')
	if cut <= 0 {
		cut = n - 1
	}
	return s[:cut+1] + "... diff cut at 64 KiB\n", true
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return itoa(n) + " " + word + "s"
}

func itoa(n int) string { return strconv.Itoa(n) }
