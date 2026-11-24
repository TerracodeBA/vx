package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/service"
)

// notePB converts a note for the caller, who decides its trust label.
func notePB(caller service.Actor, n service.Note) *vxv1.Note {
	out := &vxv1.Note{
		Id: n.ID.String(), Repo: n.RepoName, Effort: n.EffortSlug, ThreadId: n.ThreadID.String(),
		Kind: string(n.Kind), Targets: n.Targets,
		Author: principalPB(service.Actor{Principal: n.Author, Parent: n.AuthorOwner}),
		Body:   n.Body, State: n.State, AnswerBy: tsp(n.AnswerBy), CreatedAt: ts(n.CreatedAt),
		Trust: service.TrustOf(caller, n),
	}
	if n.To != nil {
		out.To = n.To.Ref()
	}
	return out
}

func notesPB(caller service.Actor, notes []service.Note) []*vxv1.Note {
	out := make([]*vxv1.Note, 0, len(notes))
	for _, n := range notes {
		out = append(out, notePB(caller, n))
	}
	return out
}

func (h *Handlers) PostNote(ctx context.Context, req *connect.Request[vxv1.PostNoteRequest]) (*connect.Response[vxv1.PostNoteResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	n, err := h.svc.PostNote(ctx, actor, service.PostNoteInput{
		Repo: m.Repo, Effort: m.Effort, Kind: service.NoteKind(strings.TrimSpace(m.Kind)), Targets: m.Targets, Body: m.Body,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.PostNoteResponse{Note: notePB(actor, n)}), nil
}

func (h *Handlers) Ask(ctx context.Context, req *connect.Request[vxv1.AskRequest]) (*connect.Response[vxv1.AskResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	n, err := h.svc.Ask(ctx, actor, service.AskInput{
		Repo: m.Repo, Effort: m.Effort, Targets: m.Targets, Body: m.Body, To: m.To,
		AnswerWithin: time.Duration(m.AnswerWithinSeconds) * time.Second,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.AskResponse{Question: notePB(actor, n)}), nil
}

func (h *Handlers) Answer(ctx context.Context, req *connect.Request[vxv1.AnswerRequest]) (*connect.Response[vxv1.AnswerResponse], error) {
	actor := actorFrom(ctx)
	a, q, err := h.svc.AnswerNote(ctx, actor, req.Msg.NoteId, req.Msg.Body)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.AnswerResponse{Answer: notePB(actor, a), Question: notePB(actor, q)}), nil
}

func (h *Handlers) ListNotes(ctx context.Context, req *connect.Request[vxv1.ListNotesRequest]) (*connect.Response[vxv1.ListNotesResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	in := service.ListNotesInput{Repo: m.Repo, Effort: m.Effort, Targets: m.Targets, Limit: int(m.Limit)}
	for _, k := range m.Kinds {
		in.Kinds = append(in.Kinds, service.NoteKind(strings.TrimSpace(k)))
	}
	if m.Since != "" {
		t, err := time.Parse(time.RFC3339, m.Since)
		if err != nil {
			return nil, invalid(fmt.Errorf("since %q is not an RFC 3339 time such as 2026-01-02T15:04:05Z", m.Since))
		}
		in.Since = t
	}
	if m.ThreadId != "" {
		id, err := uuid.Parse(m.ThreadId)
		if err != nil {
			// Accept an id prefix, as Answer does.
			n, ferr := h.svc.GetNote(ctx, actor, m.ThreadId)
			if ferr != nil {
				return nil, toConnect(ferr)
			}
			id = n.ID
		}
		in.ThreadID = id
	}
	notes, err := h.svc.ListNotes(ctx, actor, in)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.ListNotesResponse{Notes: notesPB(actor, notes)}), nil
}

func (h *Handlers) Inbox(ctx context.Context, _ *connect.Request[vxv1.InboxRequest]) (*connect.Response[vxv1.InboxResponse], error) {
	actor := actorFrom(ctx)
	in, err := h.svc.GetInbox(ctx, actor)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&vxv1.InboxResponse{
		Questions: notesPB(actor, in.Questions), Answers: notesPB(actor, in.Answers), Escalated: notesPB(actor, in.Escalated),
	}), nil
}

func (h *Handlers) Context(ctx context.Context, req *connect.Request[vxv1.ContextRequest]) (*connect.Response[vxv1.ContextResponse], error) {
	actor := actorFrom(ctx)
	m := req.Msg
	c, err := h.svc.Context(ctx, actor, m.Repo, m.Effort, m.Targets)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &vxv1.ContextResponse{Notes: notesPB(actor, c.Notes)}
	for _, w := range c.Working {
		out.Working = append(out.Working, &vxv1.ContextWorkspace{
			Owner: w.Owner.Ref(), Intent: w.Workspace.Intent, ChangeId: w.Workspace.ChangeID, CreatedAt: ts(w.Workspace.CreatedAt),
		})
	}
	for _, e := range c.RecentChanges {
		out.RecentChanges = append(out.RecentChanges, changesetRowPB(e.ChangesetRow, strings.TrimSpace(m.Effort), e.Files))
	}
	for _, cl := range c.Claims {
		out.Claims = append(out.Claims, claimPB(actor, cl))
	}
	for _, o := range c.OtherEfforts {
		out.OtherEfforts = append(out.OtherEfforts, otherEffortPB(actor, o))
	}
	return connect.NewResponse(out), nil
}

// otherEffortPB converts another effort's activity on the targets.
func otherEffortPB(actor service.Actor, o service.OtherEffortActivity) *vxv1.OtherEffort {
	pb := &vxv1.OtherEffort{
		Effort: o.Effort.Slug, Name: o.Effort.Name, Owner: o.Owner.Ref(),
		Claims: claimsPB(actor, o.Claims), Notes: notesPB(actor, o.Notes), ChangedPaths: o.ChangedPaths,
	}
	for _, w := range o.Working {
		pb.Working = append(pb.Working, &vxv1.ContextWorkspace{
			Owner: w.Owner.Ref(), Intent: w.Workspace.Intent, ChangeId: w.Workspace.ChangeID, CreatedAt: ts(w.Workspace.CreatedAt),
		})
	}
	return pb
}
