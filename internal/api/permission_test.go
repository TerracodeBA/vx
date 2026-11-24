package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// Permission errors reach API clients as CodePermissionDenied with a
// message that starts with "permission denied:" and keeps what to do next.
func TestPermissionDeniedPrefix(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	alice, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	aliceActor, _ := svc.ResolveActor(ctx, alice)
	bob, _ := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "bob"})
	aliceToken, _ := st.CreateToken(ctx, alice.ID, "test")
	bobToken, _ := st.CreateToken(ctx, bob.ID, "test")
	if _, err := svc.CreateRepo(ctx, aliceActor, service.CreateRepoInput{Name: "shop-api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrantRepo(ctx, aliceActor, "shop-api", "bob", service.RoleWrite); err != nil {
		t.Fatal(err)
	}
	eff, err := svc.CreateEffort(ctx, aliceActor, service.CreateEffortInput{Repo: "shop-api", Name: "Rate limiting"})
	if err != nil {
		t.Fatal(err)
	}
	board := func(token string) vxv1connect.BoardServiceClient {
		return vxv1connect.NewBoardServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer(token)))
	}
	posted, err := board(aliceToken).PostNote(ctx, connect.NewRequest(&vxv1.PostNoteRequest{Repo: "shop-api", Effort: eff.Slug, Kind: "plan", Body: "mine"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = board(bobToken).PostNote(ctx, connect.NewRequest(&vxv1.PostNoteRequest{
		Repo: "shop-api", Effort: eff.Slug, Kind: "plan", Body: "his", Supersedes: posted.Msg.Note.Id,
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.HasPrefix(client.Message(err), "permission denied: note ") ||
		!strings.Contains(client.Message(err), "Post a new note instead") {
		t.Fatalf("err = %v (%q)", err, client.Message(err))
	}
	// An older server's permission error without the phrase still reads with it.
	old := connect.NewError(connect.CodePermissionDenied, errorString("only its owner releases it"))
	if got := client.Message(old); got != "permission denied: only its owner releases it" {
		t.Fatalf("old server message = %q", got)
	}
	if got := (&service.Error{Code: service.CodePermission, Msg: "permission denied: twice?"}).Error(); got != "permission denied: twice?" {
		t.Fatalf("prefixed twice: %q", got)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
