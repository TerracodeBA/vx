package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
)

// Limits for object transfer. Clients batch uploads to stay under them.
const (
	MaxObjectsPerCall = 2000
	MaxMessageBytes   = 64 << 20
)

// Handlers implements every vx connect service.
type Handlers struct {
	svc *service.Service
}

// Mount registers all services on mux: PublicService without a token,
// every other service behind token authentication. CLIs older than
// version.MinCLIVersion are refused everywhere.
func Mount(mux *http.ServeMux, svc *service.Service) {
	MountWithLog(mux, svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// MountWithLog is Mount with the server's logger.
func MountWithLog(mux *http.ServeMux, svc *service.Service, log *slog.Logger) {
	h := &Handlers{svc: svc}
	limits := []connect.HandlerOption{
		connect.WithReadMaxBytes(MaxMessageBytes),
		connect.WithSendMaxBytes(MaxMessageBytes),
	}
	versions := versionInterceptor(svc, log)
	opts := append([]connect.HandlerOption{connect.WithInterceptors(versions, authInterceptor(svc))}, limits...)
	handle := func(path string, hd http.Handler) { mux.Handle(path, withRequestInfo(hd)) }
	handle(vxv1connect.NewAuthServiceHandler(h, opts...))
	handle(vxv1connect.NewObjectServiceHandler(h, opts...))
	handle(vxv1connect.NewRepoServiceHandler(h, opts...))
	handle(vxv1connect.NewEffortServiceHandler(h, opts...))
	handle(vxv1connect.NewWorkspaceServiceHandler(h, opts...))
	handle(vxv1connect.NewShipServiceHandler(h, opts...))
	handle(vxv1connect.NewBoardServiceHandler(h, opts...))
	handle(vxv1connect.NewCoordinationServiceHandler(h, opts...))
	handle(vxv1connect.NewRulesServiceHandler(h, opts...))
	handle(vxv1connect.NewOrgServiceHandler(h, opts...))
	handle(vxv1connect.NewReviewServiceHandler(h, opts...))
	// The only service without authInterceptor.
	public := append([]connect.HandlerOption{connect.WithInterceptors(versions)}, limits...)
	handle(vxv1connect.NewPublicServiceHandler(h, public...))
}

func invalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

func (h *Handlers) ref(ctx context.Context, id uuid.UUID) string {
	p, err := h.svc.Store.PrincipalByID(ctx, id)
	if err != nil {
		return ""
	}
	return p.Ref()
}

// ---------- Auth ----------

// principal is principalPB with the site admin flag filled in.
func (h *Handlers) principal(ctx context.Context, a service.Actor) *vxv1.Principal {
	out := principalPB(a)
	out.SiteAdmin = h.svc.IsSiteAdmin(ctx, a)
	return out
}

func (h *Handlers) WhoAmI(ctx context.Context, _ *connect.Request[vxv1.WhoAmIRequest]) (*connect.Response[vxv1.WhoAmIResponse], error) {
	actor := actorFrom(ctx)
	out := &vxv1.WhoAmIResponse{Principal: h.principal(ctx, actor)}
	if actor.Kind == store.KindAgent {
		// The scope is extra: who the caller is matters more than a
		// failure to read it.
		if v, err := h.svc.GetAgent(ctx, actor, ""); err == nil {
			out.Agent = h.agentInfoPB(ctx, v)
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetAgent(ctx context.Context, req *connect.Request[vxv1.GetAgentRequest]) (*connect.Response[vxv1.GetAgentResponse], error) {
	v, err := h.svc.GetAgent(ctx, actorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetAgentResponse{Agent: h.agentInfoPB(ctx, v)}), nil
}

func (h *Handlers) SetAgentScope(ctx context.Context, req *connect.Request[vxv1.SetAgentScopeRequest]) (*connect.Response[vxv1.SetAgentScopeResponse], error) {
	m := req.Msg
	opt := func(p *int32) *int {
		if p == nil {
			return nil
		}
		v := int(*p)
		return &v
	}
	v, err := h.svc.SetAgentScope(ctx, actorFrom(ctx), m.Agent, service.ScopeInput{
		Clear: m.Clear, SetAllow: m.SetAllow, Allow: m.Allow, SetDeny: m.SetDeny, Deny: m.Deny,
		MaxLandsPerDay: opt(m.MaxLandsPerDay), MaxLines: opt(m.MaxLines), MaxFiles: opt(m.MaxFiles), MaxWorkspaces: opt(m.MaxWorkspaces),
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SetAgentScopeResponse{Agent: h.agentInfoPB(ctx, v)}), nil
}

func (h *Handlers) CreateAgent(ctx context.Context, req *connect.Request[vxv1.CreateAgentRequest]) (*connect.Response[vxv1.CreateAgentResponse], error) {
	actor := actorFrom(ctx)
	agent, token, err := h.svc.CreateAgent(ctx, actor, req.Msg.Name, req.Msg.DisplayName)
	if err != nil {
		return nil, toConnect(err)
	}
	a := service.Actor{Principal: agent, Parent: &actor.Principal}
	return connect.NewResponse(&vxv1.CreateAgentResponse{Agent: h.principal(ctx, a), Token: token}), nil
}

func (h *Handlers) ListAgents(ctx context.Context, _ *connect.Request[vxv1.ListAgentsRequest]) (*connect.Response[vxv1.ListAgentsResponse], error) {
	actor := actorFrom(ctx)
	agents, err := h.svc.ListAgents(ctx, actor)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListAgentsResponse{}
	for _, a := range agents {
		out.Agents = append(out.Agents, &vxv1.AgentSummary{
			Agent:         h.principal(ctx, service.Actor{Principal: a.Agent, Parent: &actor.Principal}),
			ActiveTokens:  int32(a.ActiveTokens),
			RevokedTokens: int32(a.RevokedTokens),
			LastUsedAt:    tsp(a.LastUsedAt),
			CreatedAt:     ts(a.Agent.CreatedAt),
		})
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) ListTokens(ctx context.Context, req *connect.Request[vxv1.ListTokensRequest]) (*connect.Response[vxv1.ListTokensResponse], error) {
	actor := actorFrom(ctx)
	owner, tokens, err := h.svc.ListTokens(ctx, actor, req.Msg.Agent)
	if err != nil {
		return nil, toConnect(err)
	}
	ownerActor, err := h.svc.ResolveActor(ctx, owner)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListTokensResponse{Principal: h.principal(ctx, ownerActor)}
	for _, t := range tokens {
		out.Tokens = append(out.Tokens, tokenPB(t, owner.Ref(), actor.TokenID))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) RevokeToken(ctx context.Context, req *connect.Request[vxv1.RevokeTokenRequest]) (*connect.Response[vxv1.RevokeTokenResponse], error) {
	actor := actorFrom(ctx)
	tok, owner, already, err := h.svc.RevokeToken(ctx, actor, service.RevokeInput{ID: req.Msg.Id, AllowCurrent: req.Msg.AllowCurrent})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.RevokeTokenResponse{Token: tokenPB(tok, owner.Ref(), actor.TokenID), AlreadyRevoked: already}), nil
}

func (h *Handlers) CreateToken(ctx context.Context, req *connect.Request[vxv1.CreateTokenRequest]) (*connect.Response[vxv1.CreateTokenResponse], error) {
	actor := actorFrom(ctx)
	tok, owner, secret, err := h.svc.CreateToken(ctx, actor, req.Msg.Agent, req.Msg.Name)
	if err != nil {
		return nil, toConnect(err)
	}
	ownerActor, err := h.svc.ResolveActor(ctx, owner)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreateTokenResponse{
		Principal: h.principal(ctx, ownerActor), Token: tokenPB(tok, owner.Ref(), actor.TokenID), Secret: secret,
	}), nil
}

// ---------- Objects ----------

func (h *Handlers) MissingObjects(ctx context.Context, req *connect.Request[vxv1.MissingObjectsRequest]) (*connect.Response[vxv1.MissingObjectsResponse], error) {
	if len(req.Msg.Hashes) > 50*MaxObjectsPerCall {
		return nil, invalid(fmt.Errorf("at most %d hashes per call", 50*MaxObjectsPerCall))
	}
	hashes, err := parseHashes(req.Msg.Hashes)
	if err != nil {
		return nil, invalid(err)
	}
	missing, err := h.svc.MissingObjects(ctx, actorFrom(ctx), req.Msg.Repo, hashes)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.MissingObjectsResponse{Missing: make([]string, 0, len(missing))}
	for _, m := range missing {
		out.Missing = append(out.Missing, m.String())
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) PutObjects(ctx context.Context, req *connect.Request[vxv1.PutObjectsRequest]) (*connect.Response[vxv1.PutObjectsResponse], error) {
	if len(req.Msg.Objects) > MaxObjectsPerCall {
		return nil, invalid(fmt.Errorf("at most %d objects per call", MaxObjectsPerCall))
	}
	objs := make([]service.Object, 0, len(req.Msg.Objects))
	for _, o := range req.Msg.Objects {
		obj := service.Object{Kind: hash.Kind(o.Kind), Data: o.Data}
		if o.Hash != "" {
			hh, err := parseHash("hash", o.Hash)
			if err != nil {
				return nil, invalid(err)
			}
			obj.Hash = hh
		}
		objs = append(objs, obj)
	}
	hashes, err := h.svc.PutObjects(ctx, actorFrom(ctx), req.Msg.Repo, objs)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.PutObjectsResponse{Hashes: make([]string, 0, len(hashes))}
	for _, hh := range hashes {
		out.Hashes = append(out.Hashes, hh.String())
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetObjects(ctx context.Context, req *connect.Request[vxv1.GetObjectsRequest]) (*connect.Response[vxv1.GetObjectsResponse], error) {
	if len(req.Msg.Hashes) > MaxObjectsPerCall {
		return nil, invalid(fmt.Errorf("at most %d objects per call", MaxObjectsPerCall))
	}
	hashes, err := parseHashes(req.Msg.Hashes)
	if err != nil {
		return nil, invalid(err)
	}
	// The client asks again for what did not fit.
	objs, err := h.svc.GetObjects(ctx, actorFrom(ctx), req.Msg.Repo, hashes, MaxMessageBytes-(1<<20))
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.GetObjectsResponse{Objects: make([]*vxv1.Object, 0, len(objs))}
	for _, o := range objs {
		out.Objects = append(out.Objects, &vxv1.Object{Kind: string(o.Kind), Data: o.Data, Hash: o.Hash.String()})
	}
	return connect.NewResponse(out), nil
}

func parseHashes(in []string) ([]hash.Hash, error) {
	out := make([]hash.Hash, 0, len(in))
	for _, s := range in {
		hh, err := parseHash("hash", s)
		if err != nil {
			return nil, err
		}
		out = append(out, hh)
	}
	return out, nil
}

// ---------- Repos ----------

func (h *Handlers) CreateRepo(ctx context.Context, req *connect.Request[vxv1.CreateRepoRequest]) (*connect.Response[vxv1.CreateRepoResponse], error) {
	m := req.Msg
	repo, err := h.svc.CreateRepo(ctx, actorFrom(ctx), service.CreateRepoInput{
		Name: m.Name, Provider: store.Provider(m.Provider), Remote: m.Remote, DefaultBranch: m.DefaultBranch, Org: m.Org,
		BaseURL: m.BaseUrl, Token: m.Token,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.CreateRepoResponse{}
	if repo.Provider.Hosted() {
		// A webhook secret of its own when the server can seal one; else
		// the server's secret for the host, if any, verifies deliveries.
		hook, err := h.svc.SetRepoHost(ctx, actorFrom(ctx), service.SetRepoHostInput{Repo: repo.Name, RotateWebhookSecret: h.svc.CanSealSecrets()})
		if err != nil {
			return nil, toConnect(err)
		}
		out.Hook = hostHookPB(hook)
	}
	v, branches, err := h.svc.GetRepo(ctx, actorFrom(ctx), repo.Name)
	if err != nil {
		return nil, toConnect(err)
	}
	out.Repo = repoPB(v, branches)
	return connect.NewResponse(out), nil
}

func (h *Handlers) SetRepoHost(ctx context.Context, req *connect.Request[vxv1.SetRepoHostRequest]) (*connect.Response[vxv1.SetRepoHostResponse], error) {
	m := req.Msg
	in := service.SetRepoHostInput{Repo: m.Repo, RotateWebhookSecret: m.RotateWebhookSecret}
	if m.SetToken {
		in.Token = &m.Token
	}
	hook, err := h.svc.SetRepoHost(ctx, actorFrom(ctx), in)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SetRepoHostResponse{Hook: hostHookPB(hook)}), nil
}

func (h *Handlers) GetRepo(ctx context.Context, req *connect.Request[vxv1.GetRepoRequest]) (*connect.Response[vxv1.GetRepoResponse], error) {
	v, branches, err := h.svc.GetRepo(ctx, actorFrom(ctx), req.Msg.Name)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetRepoResponse{Repo: repoPB(v, branches)}), nil
}

func (h *Handlers) ListRepos(ctx context.Context, _ *connect.Request[vxv1.ListReposRequest]) (*connect.Response[vxv1.ListReposResponse], error) {
	repos, err := h.svc.ListRepos(ctx, actorFrom(ctx))
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListReposResponse{}
	for _, r := range repos {
		out.Repos = append(out.Repos, repoPB(r, nil))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) SyncBranch(ctx context.Context, req *connect.Request[vxv1.SyncBranchRequest]) (*connect.Response[vxv1.SyncBranchResponse], error) {
	b, n, err := h.svc.SyncBranch(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Branch)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SyncBranchResponse{Branch: branchPB(b), Imported: int32(n)}), nil
}

// ---------- Efforts ----------

func (h *Handlers) CreateEffort(ctx context.Context, req *connect.Request[vxv1.CreateEffortRequest]) (*connect.Response[vxv1.CreateEffortResponse], error) {
	m := req.Msg
	eff, err := h.svc.CreateEffort(ctx, actorFrom(ctx), service.CreateEffortInput{Repo: m.Repo, Name: m.Name, Goal: m.Goal, BaseBranch: m.BaseBranch})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.CreateEffortResponse{Effort: effortPB(m.Repo, eff, h.ref(ctx, eff.OwnerID), 0)}), nil
}

func (h *Handlers) GetEffort(ctx context.Context, req *connect.Request[vxv1.GetEffortRequest]) (*connect.Response[vxv1.GetEffortResponse], error) {
	v, err := h.svc.GetEffort(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.GetEffortResponse{Effort: effortPB(v.Repo.Name, v.Effort, h.ref(ctx, v.Effort.OwnerID), len(v.Workspaces))}
	out.Effort.Rebase = h.activeRebasePB(ctx, v)
	for _, w := range v.Workspaces {
		out.OpenWorkspaces = append(out.OpenWorkspaces, workspacePB(v.Repo.Name, v.Effort.Slug, w, h.ref(ctx, w.OwnerID)))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) ListEfforts(ctx context.Context, req *connect.Request[vxv1.ListEffortsRequest]) (*connect.Response[vxv1.ListEffortsResponse], error) {
	repo, effs, err := h.svc.ListEfforts(ctx, actorFrom(ctx), req.Msg.Repo, store.EffortState(req.Msg.State))
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ListEffortsResponse{}
	for _, e := range effs {
		open, _ := h.svc.Store.ListWorkspaces(ctx, e.ID, store.WorkspaceOpen)
		out.Efforts = append(out.Efforts, effortPB(repo.Name, e, h.ref(ctx, e.OwnerID), len(open)))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) Log(ctx context.Context, req *connect.Request[vxv1.LogRequest]) (*connect.Response[vxv1.LogResponse], error) {
	entries, err := h.svc.Log(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort, int(req.Msg.Limit), req.Msg.IncludeFiles)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.LogResponse{}
	for _, e := range entries {
		files := e.Files
		if !req.Msg.IncludeFiles {
			files = nil
		}
		out.Changesets = append(out.Changesets, changesetRowPB(e.ChangesetRow, req.Msg.Effort, files))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetChangeset(ctx context.Context, req *connect.Request[vxv1.GetChangesetRequest]) (*connect.Response[vxv1.GetChangesetResponse], error) {
	_, e, err := h.svc.GetChangeset(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Ref)
	if err != nil {
		return nil, toConnect(err)
	}
	effort := ""
	if e.EffortID != nil {
		if eff, err := h.svc.Store.EffortByID(ctx, *e.EffortID); err == nil {
			effort = eff.Slug
		}
	}
	return connect.NewResponse(&vxv1.GetChangesetResponse{Changeset: changesetRowPB(e.ChangesetRow, effort, e.Files)}), nil
}

// ---------- Workspaces ----------

func parseWorkspaceID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, invalid(fmt.Errorf("workspace id %q: %w", s, err))
	}
	return id, nil
}

func (h *Handlers) workspaceResp(ctx context.Context, v service.WorkspaceView) *vxv1.Workspace {
	return workspacePB(v.Repo.Name, v.Effort.Slug, v.Workspace, h.ref(ctx, v.Workspace.OwnerID))
}

func (h *Handlers) OpenWorkspace(ctx context.Context, req *connect.Request[vxv1.OpenWorkspaceRequest]) (*connect.Response[vxv1.OpenWorkspaceResponse], error) {
	v, err := h.svc.OpenWorkspace(ctx, actorFrom(ctx), req.Msg.Repo, req.Msg.Effort, req.Msg.Intent)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.OpenWorkspaceResponse{Workspace: h.workspaceResp(ctx, v)}), nil
}

func (h *Handlers) GetWorkspace(ctx context.Context, req *connect.Request[vxv1.GetWorkspaceRequest]) (*connect.Response[vxv1.GetWorkspaceResponse], error) {
	id, err := parseWorkspaceID(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetWorkspace(ctx, actorFrom(ctx), id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetWorkspaceResponse{Workspace: h.workspaceResp(ctx, v)}), nil
}

func (h *Handlers) SetBase(ctx context.Context, req *connect.Request[vxv1.SetBaseRequest]) (*connect.Response[vxv1.SetBaseResponse], error) {
	id, err := parseWorkspaceID(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	base, err := parseHash("base", req.Msg.Base)
	if err != nil {
		return nil, invalid(err)
	}
	v, err := h.svc.SetBase(ctx, actorFrom(ctx), id, base)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.SetBaseResponse{Workspace: h.workspaceResp(ctx, v)}), nil
}

func (h *Handlers) Land(ctx context.Context, req *connect.Request[vxv1.LandRequest]) (*connect.Response[vxv1.LandResponse], error) {
	m := req.Msg
	id, err := parseWorkspaceID(m.WorkspaceId)
	if err != nil {
		return nil, err
	}
	base, err := parseHash("base", m.Base)
	if err != nil {
		return nil, invalid(err)
	}
	tree, err := parseHash("tree", m.Tree)
	if err != nil {
		return nil, invalid(err)
	}
	ev, err := evidenceFromPB(m.Evidence)
	if err != nil {
		return nil, invalid(err)
	}
	res, err := h.svc.Land(ctx, actorFrom(ctx), service.LandInput{
		WorkspaceID: id, Base: base, Tree: tree, Intent: m.Intent, Evidence: ev,
		DependsOn: m.DependsOn, Model: m.Model, Session: m.Session, RequireUpToDate: m.RequireUpToDate,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	effort := ""
	if v, err := h.svc.GetWorkspace(ctx, actorFrom(ctx), id); err == nil {
		effort = v.Effort.Slug
	}
	out := landResponsePB(actorFrom(ctx), res, effort)
	return connect.NewResponse(out), nil
}

func (h *Handlers) Abandon(ctx context.Context, req *connect.Request[vxv1.AbandonRequest]) (*connect.Response[vxv1.AbandonResponse], error) {
	id, err := parseWorkspaceID(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.Abandon(ctx, actorFrom(ctx), id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.AbandonResponse{Workspace: h.workspaceResp(ctx, v)}), nil
}

func (h *Handlers) ReportChanges(ctx context.Context, req *connect.Request[vxv1.ReportChangesRequest]) (*connect.Response[vxv1.ReportChangesResponse], error) {
	id, err := parseWorkspaceID(req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if err := h.svc.ReportChanges(ctx, actorFrom(ctx), id, req.Msg.Paths); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ReportChangesResponse{}), nil
}
