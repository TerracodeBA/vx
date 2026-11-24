package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/gen/vx/v1/vxv1connect"
	"github.com/TerracodeBA/vx/internal/api"
	"github.com/TerracodeBA/vx/internal/blob"
	"github.com/TerracodeBA/vx/internal/service"
	"github.com/TerracodeBA/vx/internal/store"
	"github.com/TerracodeBA/vx/internal/store/storetest"
)

// bearer adds a token to every request.
type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b != "" {
			req.Header().Set("Authorization", "Bearer "+string(b))
		}
		return next(ctx, req)
	}
}
func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func TestBoardServiceOverConnect(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	svc := service.New(st, blob.NewMem(), nil)
	mux := http.NewServeMux()
	api.Mount(mux, svc)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	alice, err := st.CreatePrincipal(ctx, store.Principal{Kind: store.KindHuman, Handle: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	aliceActor, _ := svc.ResolveActor(ctx, alice)
	agent, agentToken, err := svc.CreateAgent(ctx, aliceActor, "backend", "")
	if err != nil {
		t.Fatal(err)
	}
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
	client := func(token string) vxv1connect.BoardServiceClient {
		return vxv1connect.NewBoardServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer(token)))
	}
	asAgent, asAlice, asBob := client(agentToken), client(aliceToken), client(bobToken)

	if _, err := client("").Inbox(ctx, connect.NewRequest(&vxv1.InboxRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token err = %v", err)
	}

	posted, err := asAgent.PostNote(ctx, connect.NewRequest(&vxv1.PostNoteRequest{
		Repo: "shop-api", Effort: eff.Slug, Kind: "plan", Targets: []string{"internal/http/"}, Body: "limit logins",
	}))
	if err != nil {
		t.Fatal(err)
	}
	n := posted.Msg.Note
	if n.Kind != "plan" || n.Repo != "shop-api" || n.Effort != eff.Slug || n.ThreadId != n.Id || n.Trust != service.TrustSelf ||
		n.Author.Ref != agent.Ref() || n.Author.OnBehalfOf != "user:alice" || n.State != "open" || n.AnswerBy != "" {
		t.Fatalf("note = %+v", n)
	}
	if _, err := time.Parse(time.RFC3339, n.CreatedAt); err != nil {
		t.Fatalf("created_at %q: %v", n.CreatedAt, err)
	}
	if _, err := asAgent.PostNote(ctx, connect.NewRequest(&vxv1.PostNoteRequest{Repo: "shop-api", Effort: eff.Slug, Kind: "question", Body: "x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("question through PostNote err = %v", err)
	}

	asked, err := asAgent.Ask(ctx, connect.NewRequest(&vxv1.AskRequest{
		Repo: "shop-api", Effort: eff.Slug, Body: "5 or 10?", To: "user:bob", AnswerWithinSeconds: 600, Targets: []string{"internal/http/limit.go"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	q := asked.Msg.Question
	answerBy, err := time.Parse(time.RFC3339, q.AnswerBy)
	if err != nil || q.To != "user:bob" || q.Kind != "question" {
		t.Fatalf("question = %+v (%v)", q, err)
	}
	created, _ := time.Parse(time.RFC3339, q.CreatedAt)
	if d := answerBy.Sub(created); d != 10*time.Minute {
		t.Fatalf("answer_by - created_at = %s", d)
	}

	inbox, err := asBob.Inbox(ctx, connect.NewRequest(&vxv1.InboxRequest{}))
	if err != nil || len(inbox.Msg.Questions) != 1 || inbox.Msg.Questions[0].Id != q.Id || inbox.Msg.Questions[0].Trust != service.TrustTeammate {
		t.Fatalf("bob inbox = %+v, %v", inbox.Msg, err)
	}

	answered, err := asBob.Answer(ctx, connect.NewRequest(&vxv1.AnswerRequest{NoteId: q.Id[:8], Body: "5"}))
	if err != nil {
		t.Fatal(err)
	}
	if answered.Msg.Answer.ThreadId != q.Id || answered.Msg.Answer.Trust != service.TrustSelf || answered.Msg.Question.State != "answered" {
		t.Fatalf("answer = %+v", answered.Msg)
	}

	// Alice sees her agent's notes as hers; the thread lists by prefix too.
	list, err := asAlice.ListNotes(ctx, connect.NewRequest(&vxv1.ListNotesRequest{Repo: "shop-api", Effort: eff.Slug, ThreadId: q.Id[:8]}))
	if err != nil || len(list.Msg.Notes) != 2 || list.Msg.Notes[1].Trust != service.TrustSamePerson || list.Msg.Notes[0].Trust != service.TrustTeammate {
		t.Fatalf("thread = %+v, %v", list.Msg, err)
	}
	list, err = asAlice.ListNotes(ctx, connect.NewRequest(&vxv1.ListNotesRequest{Repo: "shop-api", Effort: eff.Slug, Kinds: []string{"plan"}, Since: n.CreatedAt}))
	if err != nil || len(list.Msg.Notes) != 1 || list.Msg.Notes[0].Id != n.Id {
		t.Fatalf("plans = %+v, %v", list.Msg, err)
	}
	if _, err := asAlice.ListNotes(ctx, connect.NewRequest(&vxv1.ListNotesRequest{Repo: "shop-api", Effort: eff.Slug, Since: "yesterday"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad since err = %v", err)
	}

	mine, err := asAgent.Inbox(ctx, connect.NewRequest(&vxv1.InboxRequest{}))
	if err != nil || len(mine.Msg.Answers) != 1 || mine.Msg.Answers[0].Body != "5" {
		t.Fatalf("agent inbox = %+v, %v", mine.Msg, err)
	}

	ctxResp, err := asBob.Context(ctx, connect.NewRequest(&vxv1.ContextRequest{Repo: "shop-api", Effort: eff.Slug, Targets: []string{"internal/http/limit.go"}}))
	if err != nil || len(ctxResp.Msg.Notes) != 3 {
		t.Fatalf("context = %+v, %v", ctxResp.Msg, err)
	}
	if _, err := asBob.Context(ctx, connect.NewRequest(&vxv1.ContextRequest{Repo: "shop-api", Effort: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown effort err = %v", err)
	}
}
