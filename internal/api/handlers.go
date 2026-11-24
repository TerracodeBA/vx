package api

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
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

// Mount registers all services on mux behind token authentication.
func Mount(mux *http.ServeMux, svc *service.Service) {
	h := &Handlers{svc: svc}
	opts := []connect.HandlerOption{
		connect.WithInterceptors(authInterceptor(svc)),
		connect.WithReadMaxBytes(MaxMessageBytes),
		connect.WithSendMaxBytes(MaxMessageBytes),
	}
	mux.Handle(vxv1connect.NewAuthServiceHandler(h, opts...))
	mux.Handle(vxv1connect.NewObjectServiceHandler(h, opts...))
	mux.Handle(vxv1connect.NewRepoServiceHandler(h, opts...))
	mux.Handle(vxv1connect.NewEffortServiceHandler(h, opts...))
	mux.Handle(vxv1connect.NewWorkspaceServiceHandler(h, opts...))
	mux.Handle(vxv1connect.NewShipServiceHandler(h, opts...))
	mux.Handle(vxv1connect.NewBoardServiceHandler(h, opts...))
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

func (h *Handlers) WhoAmI(ctx context.Context, _ *connect.Request[vxv1.WhoAmIRequest]) (*connect.Response[vxv1.WhoAmIResponse], error) {
	return connect.NewResponse(&vxv1.WhoAmIResponse{Principal: principalPB(actorFrom(ctx))}), nil
}

func (h *Handlers) CreateAgent(ctx context.Context, req *connect.Request[vxv1.CreateAgentRequest]) (*connect.Response[vxv1.CreateAgentResponse], error) {
	actor := actorFrom(ctx)
	agent, token, err := h.svc.CreateAgent(ctx, actor, req.Msg.Name, req.Msg.DisplayName)
	if err != nil {
		return nil, toConnect(err)
	}
	a := service.Actor{Principal: agent, Parent: &actor.Principal}
	return connect.NewResponse(&vxv1.CreateAgentResponse{Agent: principalPB(a), Token: token}), nil
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
			Agent:         principalPB(service.Actor{Principal: a.Agent, Parent: &actor.Principal}),
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
	out := &vxv1.ListTokensResponse{Principal: principalPB(ownerActor)}
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
		Principal: principalPB(ownerActor), Token: tokenPB(tok, owner.Ref(), actor.TokenID), Secret: secret,
	}), nil
}

// ---------- Objects ----------

func (h *Handlers) MissingObjects(ctx context.Context, req *connect.Request[vxv1.MissingObjectsRequest]) (*connect.Response[vxv1.MissingObjectsResponse], error) {
	if len(req.Msg.Hashes) > 50*MaxObjectsPerCall {
		return nil, invalid(fmt.Errorf("at most %d hashes per call", 50*MaxObjectsPerCall))
	}
	out := &vxv1.MissingObjectsResponse{}
	for _, s := range req.Msg.Hashes {
		hh, err := parseHash("hash", s)
		if err != nil {
			return nil, invalid(err)
		}
		ok, err := h.svc.Blobs.Has(ctx, hh)
		if err != nil {
			return nil, toConnect(err)
		}
		if !ok {
			out.Missing = append(out.Missing, s)
		}
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) PutObjects(ctx context.Context, req *connect.Request[vxv1.PutObjectsRequest]) (*connect.Response[vxv1.PutObjectsResponse], error) {
	if len(req.Msg.Objects) > MaxObjectsPerCall {
		return nil, invalid(fmt.Errorf("at most %d objects per call", MaxObjectsPerCall))
	}
	out := &vxv1.PutObjectsResponse{}
	for _, o := range req.Msg.Objects {
		kind := hash.Kind(o.Kind)
		if kind != hash.KindBlob && kind != hash.KindTree {
			return nil, invalid(fmt.Errorf("object kind must be blob or tree, got %q", o.Kind))
		}
		if kind == hash.KindTree {
			if err := validTree(o.Data); err != nil {
				return nil, invalid(err)
			}
		}
		hh, err := h.svc.Blobs.Put(ctx, kind, o.Data)
		if err != nil {
			return nil, toConnect(err)
		}
		if o.Hash != "" && o.Hash != hh.String() {
			return nil, invalid(fmt.Errorf("object hash mismatch: client says %s, content is %s", o.Hash, hh))
		}
		out.Hashes = append(out.Hashes, hh.String())
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) GetObjects(ctx context.Context, req *connect.Request[vxv1.GetObjectsRequest]) (*connect.Response[vxv1.GetObjectsResponse], error) {
	if len(req.Msg.Hashes) > MaxObjectsPerCall {
		return nil, invalid(fmt.Errorf("at most %d objects per call", MaxObjectsPerCall))
	}
	out := &vxv1.GetObjectsResponse{}
	size := 0
	for _, s := range req.Msg.Hashes {
		hh, err := parseHash("hash", s)
		if err != nil {
			return nil, invalid(err)
		}
		kind, data, err := h.svc.Blobs.Get(ctx, hh)
		if err != nil {
			return nil, toConnect(service.Classify(err))
		}
		size += len(data)
		if size > MaxMessageBytes-(1<<20) && len(out.Objects) > 0 {
			// The client asks again for what did not fit.
			break
		}
		out.Objects = append(out.Objects, &vxv1.Object{Kind: string(kind), Data: data, Hash: s})
	}
	return connect.NewResponse(out), nil
}

// ---------- Repos ----------

func (h *Handlers) CreateRepo(ctx context.Context, req *connect.Request[vxv1.CreateRepoRequest]) (*connect.Response[vxv1.CreateRepoResponse], error) {
	m := req.Msg
	repo, err := h.svc.CreateRepo(ctx, actorFrom(ctx), service.CreateRepoInput{
		Name: m.Name, Provider: store.Provider(m.Provider), Remote: m.Remote, DefaultBranch: m.DefaultBranch,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	branches, _ := h.svc.Store.ListBranches(ctx, repo.ID)
	return connect.NewResponse(&vxv1.CreateRepoResponse{Repo: repoPB(repo, branches)}), nil
}

func (h *Handlers) GetRepo(ctx context.Context, req *connect.Request[vxv1.GetRepoRequest]) (*connect.Response[vxv1.GetRepoResponse], error) {
	repo, err := h.svc.Store.RepoByName(ctx, req.Msg.Name)
	if err != nil {
		return nil, toConnect(service.Classify(err))
	}
	branches, err := h.svc.Store.ListBranches(ctx, repo.ID)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.GetRepoResponse{Repo: repoPB(repo, branches)}), nil
}

func (h *Handlers) ListRepos(ctx context.Context, _ *connect.Request[vxv1.ListReposRequest]) (*connect.Response[vxv1.ListReposResponse], error) {
	repos, err := h.svc.Store.ListRepos(ctx)
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
	v, err := h.svc.GetEffort(ctx, req.Msg.Repo, req.Msg.Effort)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.GetEffortResponse{Effort: effortPB(v.Repo.Name, v.Effort, h.ref(ctx, v.Effort.OwnerID), len(v.Workspaces))}
	for _, w := range v.Workspaces {
		out.OpenWorkspaces = append(out.OpenWorkspaces, workspacePB(v.Repo.Name, v.Effort.Slug, w, h.ref(ctx, w.OwnerID)))
	}
	return connect.NewResponse(out), nil
}

func (h *Handlers) ListEfforts(ctx context.Context, req *connect.Request[vxv1.ListEffortsRequest]) (*connect.Response[vxv1.ListEffortsResponse], error) {
	repo, effs, err := h.svc.ListEfforts(ctx, req.Msg.Repo, store.EffortState(req.Msg.State))
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
	entries, err := h.svc.Log(ctx, req.Msg.Repo, req.Msg.Effort, int(req.Msg.Limit), req.Msg.IncludeFiles)
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
	_, e, err := h.svc.GetChangeset(ctx, req.Msg.Repo, req.Msg.Ref)
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
	v, err := h.svc.GetWorkspace(ctx, id)
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
	out := &vxv1.LandResponse{
		Landed: res.Landed, Rebased: res.Rebased, AutoMerged: res.AutoMerged,
		Conflicts: conflictsPB(res.Conflicts), EffortHead: res.EffortHead.String(), EffortMoved: res.EffortMoved,
		Warnings: warningsPB(res.Warnings),
	}
	if res.Landed {
		effort := ""
		if v, err := h.svc.GetWorkspace(ctx, id); err == nil {
			effort = v.Effort.Slug
		}
		out.Changeset = changesetRowPB(res.Changeset, effort, res.Files)
	}
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

// validTree rejects malformed tree objects before they are stored.
func validTree(data []byte) error {
	if _, err := object.DecodeTree(data); err != nil {
		return fmt.Errorf("invalid tree object: %w", err)
	}
	return nil
}
