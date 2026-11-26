// Package client is the Go client for a vx server: typed service clients
// plus batched object transfer between a local blob store and the server.
package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/version"
)

// Client talks to one vx server as one principal.
type Client struct {
	Auth       vxv1connect.AuthServiceClient
	Objects    vxv1connect.ObjectServiceClient
	Repos      vxv1connect.RepoServiceClient
	Efforts    vxv1connect.EffortServiceClient
	Workspaces vxv1connect.WorkspaceServiceClient
	Ships      vxv1connect.ShipServiceClient
	Board      vxv1connect.BoardServiceClient
	Coord      vxv1connect.CoordinationServiceClient
	Orgs       vxv1connect.OrgServiceClient
	Rules      vxv1connect.RulesServiceClient
	Reviews    vxv1connect.ReviewServiceClient
	Queue      vxv1connect.CheckQueueServiceClient
	Checks     vxv1connect.CheckServiceClient
	Public     vxv1connect.PublicServiceClient // needs no token
}

const maxMessage = 64 << 20

// New returns a client for baseURL that sends token on every call (none
// when token is empty: only Public works then) and this vx's version, so
// the server can refuse versions it no longer works with.
func New(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	auth := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			req.Header().Set("User-Agent", "vx-cli/"+version.Version)
			req.Header().Set(version.Header, version.Version)
			return next(ctx, req)
		}
	})
	opts := []connect.ClientOption{
		connect.WithInterceptors(auth),
		connect.WithReadMaxBytes(maxMessage),
		connect.WithSendMaxBytes(maxMessage),
		connect.WithSendGzip(),
	}
	return &Client{
		Auth:       vxv1connect.NewAuthServiceClient(httpClient, baseURL, opts...),
		Objects:    vxv1connect.NewObjectServiceClient(httpClient, baseURL, opts...),
		Repos:      vxv1connect.NewRepoServiceClient(httpClient, baseURL, opts...),
		Efforts:    vxv1connect.NewEffortServiceClient(httpClient, baseURL, opts...),
		Workspaces: vxv1connect.NewWorkspaceServiceClient(httpClient, baseURL, opts...),
		Ships:      vxv1connect.NewShipServiceClient(httpClient, baseURL, opts...),
		Board:      vxv1connect.NewBoardServiceClient(httpClient, baseURL, opts...),
		Coord:      vxv1connect.NewCoordinationServiceClient(httpClient, baseURL, opts...),
		Orgs:       vxv1connect.NewOrgServiceClient(httpClient, baseURL, opts...),
		Rules:      vxv1connect.NewRulesServiceClient(httpClient, baseURL, opts...),
		Reviews:    vxv1connect.NewReviewServiceClient(httpClient, baseURL, opts...),
		Queue:      vxv1connect.NewCheckQueueServiceClient(httpClient, baseURL, opts...),
		Checks:     vxv1connect.NewCheckServiceClient(httpClient, baseURL, opts...),
		Public:     vxv1connect.NewPublicServiceClient(httpClient, baseURL, opts...),
	}
}

// Message returns the server's message for a connect error, without the
// "code:" prefix, for showing to people.
func Message(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		if ce.Code() == connect.CodePermissionDenied && !strings.HasPrefix(ce.Message(), permissionDenied) {
			return permissionDenied + ce.Message() // servers before v0.3 left the phrase out
		}
		return ce.Message()
	}
	return err.Error()
}

// permissionDenied starts every permission error (service.PermissionDenied).
const permissionDenied = "permission denied: "

// Code returns the connect code of err, or 0.
func Code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return 0
}

// Batch sizes for object transfer.
const (
	batchObjects = 1000
	batchBytes   = 16 << 20
)

// Upload sends the objects in hashes that are not part of repo on the
// server yet. Objects are scoped to repositories: content the server stores
// for another repository is sent again (storage dedupes it), which proves
// the caller has it.
func (c *Client) Upload(ctx context.Context, repo string, local blob.Store, hashes []hash.Hash) (int, error) {
	if len(hashes) == 0 {
		return 0, nil
	}
	var missing []string
	for start := 0; start < len(hashes); start += 20000 {
		end := min(start+20000, len(hashes))
		req := &vxv1.MissingObjectsRequest{Repo: repo}
		for _, h := range hashes[start:end] {
			req.Hashes = append(req.Hashes, h.String())
		}
		resp, err := c.Objects.MissingObjects(ctx, connect.NewRequest(req))
		if err != nil {
			return 0, err
		}
		missing = append(missing, resp.Msg.Missing...)
	}
	sent := 0
	batch := &vxv1.PutObjectsRequest{Repo: repo}
	size := 0
	flush := func() error {
		if len(batch.Objects) == 0 {
			return nil
		}
		if _, err := c.Objects.PutObjects(ctx, connect.NewRequest(batch)); err != nil {
			return err
		}
		sent += len(batch.Objects)
		batch = &vxv1.PutObjectsRequest{Repo: repo}
		size = 0
		return nil
	}
	for _, s := range missing {
		h, err := hash.Parse(s)
		if err != nil {
			return sent, err
		}
		kind, data, err := local.Get(ctx, h)
		if err != nil {
			return sent, fmt.Errorf("local object %s: %w", h.Short(), err)
		}
		if size+len(data) > batchBytes || len(batch.Objects) >= batchObjects {
			if err := flush(); err != nil {
				return sent, err
			}
		}
		batch.Objects = append(batch.Objects, &vxv1.Object{Kind: string(kind), Data: data, Hash: s})
		size += len(data)
	}
	return sent, flush()
}

// Download fetches objects of repo into local, skipping those already
// there. Every object is verified against its hash on the way in.
func (c *Client) Download(ctx context.Context, repo string, local blob.Store, hashes []hash.Hash) error {
	var need []string
	for _, h := range hashes {
		ok, err := local.Has(ctx, h)
		if err != nil {
			return err
		}
		if !ok {
			need = append(need, h.String())
		}
	}
	for len(need) > 0 {
		n := min(len(need), batchObjects)
		resp, err := c.Objects.GetObjects(ctx, connect.NewRequest(&vxv1.GetObjectsRequest{Repo: repo, Hashes: need[:n]}))
		if err != nil {
			return err
		}
		if len(resp.Msg.Objects) == 0 {
			return fmt.Errorf("server returned no objects for %s", need[0])
		}
		for _, o := range resp.Msg.Objects {
			got, err := local.Put(ctx, hash.Kind(o.Kind), o.Data)
			if err != nil {
				return err
			}
			if got.String() != o.Hash {
				return fmt.Errorf("server sent wrong content for %s", o.Hash)
			}
		}
		need = need[len(resp.Msg.Objects):]
	}
	return nil
}

// FetchTree downloads every tree and blob under root, a tree of repo, that
// local lacks, level by level.
func (c *Client) FetchTree(ctx context.Context, repo string, local blob.Store, root hash.Hash) error {
	level := []hash.Hash{root}
	for len(level) > 0 {
		if err := c.Download(ctx, repo, local, level); err != nil {
			return err
		}
		var next, blobs []hash.Hash
		for _, h := range level {
			t, err := object.GetTree(ctx, local, h)
			if err != nil {
				return err
			}
			for _, e := range t.Entries {
				if e.Mode == object.ModeDir {
					next = append(next, e.Hash)
				} else {
					blobs = append(blobs, e.Hash)
				}
			}
		}
		if err := c.Download(ctx, repo, local, blobs); err != nil {
			return err
		}
		level = next
	}
	return nil
}
