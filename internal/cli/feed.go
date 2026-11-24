package cli

// The board feed: notes and claims brought to the agent in the commands it
// already runs (vx sync, land, status, wait), so nobody has to poll vx
// board. A working copy remembers when it was last shown the board
// (Config.BoardSeenAt) and is shown each note once.

import (
	"context"
	"encoding/json"
	"strings"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

const (
	// feedShown is how many new notes sync and land print in full.
	feedShown = 5
	// feedFetch is how many new notes one look at the board reads.
	feedFetch = 100
	// contextBatch is the most targets one Context request takes.
	contextBatch = 20
)

// boardFeed is what the board has for a working copy right now.
type boardFeed struct {
	notes  []*vxv1.Note  // new notes for the agent, newest first
	claims []*vxv1.Claim // active claims of others on the agent's files
	newest string        // created_at of the newest note read, to mark as seen
	me     string        // the caller's ref, shown as "you"
}

// json is the "board" object of the sync and land results. Both lists are
// always present (empty when the server has no board or no claims).
func (f boardFeed) json() map[string]any {
	notes := make([]json.RawMessage, 0, len(f.notes))
	for _, n := range f.notes {
		if data, err := pj.Marshal(n); err == nil {
			notes = append(notes, data)
		}
	}
	claims := make([]json.RawMessage, 0, len(f.claims))
	for _, c := range f.claims {
		if data, err := pj.Marshal(c); err == nil {
			claims = append(claims, data)
		}
	}
	return map[string]any{"new_notes": notes, "claims": claims}
}

// changedPaths lists the paths of changes.
func changedPaths(changes []object.Change) []string {
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, ch.Path)
	}
	return out
}

// after reports whether RFC 3339 time a is strictly after b; anything is
// after an empty or unreadable b.
func after(a, b string) bool {
	tb, ok := parseTime(b)
	if !ok {
		return true
	}
	ta, ok := parseTime(a)
	return ok && ta.After(tb)
}

// forMe reports whether a note others wrote belongs in the agent's feed:
// a decision or interface anywhere in the effort, a question to the agent,
// or any note on one of paths.
func forMe(n *vxv1.Note, me string, paths []string) bool {
	if n.Trust == "self" || (me != "" && n.Author.GetRef() == me) {
		return false
	}
	if n.Kind == "decision" || n.Kind == "interface" {
		return true
	}
	if me != "" && n.To == me {
		return true
	}
	return onPaths(n.Targets, paths)
}

func onPaths(targets, paths []string) bool {
	for _, t := range targets {
		for _, p := range paths {
			if targetsMatch(t, p) {
				return true
			}
		}
	}
	return false
}

// newNotes reads the notes that arrived since the working copy last saw
// the board and keeps those for the agent. ok is false when the server
// has no board.
func (a *App) newNotes(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, me string, paths []string) (notes []*vxv1.Note, newest string, ok bool) {
	seen := wc.Config.BoardSeenAt
	resp, err := c.Board.ListNotes(ctx, connect.NewRequest(&vxv1.ListNotesRequest{
		Repo: wc.Config.Repo, Effort: wc.Config.Effort, Since: seen, Limit: feedFetch,
	}))
	if err != nil {
		return nil, seen, false
	}
	newest = seen
	for _, n := range resp.Msg.Notes { // newest first
		if !after(n.CreatedAt, seen) { // Since is inclusive
			continue
		}
		if after(n.CreatedAt, newest) {
			newest = n.CreatedAt
		}
		if forMe(n, me, paths) {
			notes = append(notes, n)
		}
	}
	return notes, newest, true
}

// othersClaims returns the active claims of others on paths, from Context.
// Servers without claims return none.
func (a *App) othersClaims(ctx context.Context, c *client.Client, repo, slug, me string, paths []string) ([]*vxv1.Claim, []*vxv1.Note, bool) {
	var claims []*vxv1.Claim
	var notes []*vxv1.Note
	have := map[string]bool{}
	haveNote := map[string]bool{}
	for start := 0; start < len(paths); start += contextBatch {
		end := min(start+contextBatch, len(paths))
		resp, err := c.Board.Context(ctx, connect.NewRequest(&vxv1.ContextRequest{Repo: repo, Effort: slug, Targets: paths[start:end]}))
		if err != nil {
			return claims, notes, false
		}
		for _, cl := range resp.Msg.Claims {
			if have[cl.Id] || cl.Trust == "self" || (me != "" && cl.Owner.GetRef() == me) {
				continue
			}
			have[cl.Id] = true
			claims = append(claims, cl)
		}
		for _, n := range resp.Msg.Notes {
			if !haveNote[n.Id] {
				haveNote[n.Id] = true
				notes = append(notes, n)
			}
		}
	}
	return claims, notes, true
}

// markBoardSeen records that the working copy was shown the board up to
// newest.
func markBoardSeen(wc *workcopy.WorkingCopy, newest string) {
	if newest == "" || !after(newest, wc.Config.BoardSeenAt) {
		return
	}
	wc.Config.BoardSeenAt = newest
	_ = wc.Save() // best effort: the worst case is seeing a note again
}

// boardFeedFor reads the feed for paths: the files changed locally (sync)
// or just landed (land). With withClaims, it also reads others' claims on
// them. It marks the notes it returns as seen.
func (a *App) boardFeedFor(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, paths []string, withClaims bool) boardFeed {
	me := a.me(ctx, c)
	f := boardFeed{me: me}
	notes, newest, ok := a.newNotes(ctx, c, wc, me, paths)
	if !ok {
		return f
	}
	f.notes, f.newest = notes, newest
	if withClaims && len(paths) > 0 {
		f.claims, _, _ = a.othersClaims(ctx, c, wc.Config.Repo, wc.Config.Effort, me, paths)
	}
	markBoardSeen(wc, newest)
	return f
}

// printBoardFeed prints the board section that ends vx sync and vx land:
// new notes for the agent, then others' claims on its files. It prints
// nothing when there is nothing new.
func (a *App) printBoardFeed(f boardFeed) {
	me := f.me
	if len(f.notes) > 0 {
		a.printf("\nNew on the board since your last sync (%d). %s\n", len(f.notes), boardFraming)
		roots, replies := threads(f.notes)
		shown := 0
		for _, n := range roots {
			if shown == feedShown {
				break
			}
			a.printNote(n, noteOpts{me: me, indent: "  "})
			shown++
			for _, r := range replies[n.Id] {
				if shown == feedShown {
					break
				}
				a.printNote(r, noteOpts{me: me, indent: "      "})
				shown++
			}
		}
		if more := len(f.notes) - shown; more > 0 {
			a.printf("  %d more: vx board\n", more)
		}
		a.printf("  Read these before you continue.\n")
	}
	if len(f.claims) > 0 {
		a.printf("\nClaimed by others (%d): they are changing these now; coordinate before you land.\n", len(f.claims))
		for _, cl := range f.claims {
			a.printClaim(cl, "  ")
		}
	}
}

// printClaim prints a claim like a note: trust label, owner, targets,
// expiry, then the reason under a gutter.
func (a *App) printClaim(cl *vxv1.Claim, indent string) {
	trust := cl.Trust
	if trust == "" {
		trust = "teammate"
	}
	var b strings.Builder
	b.WriteString("[" + trust + "] claim  " + cl.Owner.GetRef())
	if len(cl.Targets) > 0 {
		b.WriteString("  on " + strings.Join(cl.Targets, ", "))
	}
	if cl.ExpiresAt != "" {
		b.WriteString("  until " + clock(cl.ExpiresAt) + " (" + expiresIn(cl.ExpiresAt) + ")")
	}
	b.WriteString("  id " + noteID(cl.Id))
	a.printf("%s%s\n", indent, b.String())
	if reason := cleanBody(cl.Reason); reason != "" {
		for _, line := range strings.Split(reason, "\n") {
			a.printf("%s  │ %s\n", indent, line)
		}
	}
}

// claimLine is a claim on one line, for vx status.
func claimLine(cl *vxv1.Claim) string {
	trust := cl.Trust
	if trust == "" {
		trust = "teammate"
	}
	s := "[" + trust + "] " + cl.Owner.GetRef() + " on " + strings.Join(cl.Targets, ", ")
	if cl.ExpiresAt != "" {
		s += " until " + clock(cl.ExpiresAt)
	}
	if r := oneLine(cleanBody(cl.Reason)); r != "" {
		s += ": " + clip(r, 60)
	}
	return s
}

// syncFeed prints the board section after a sync and returns its JSON:
// new notes for the agent and others' claims on the files it changed
// locally.
func (a *App) syncFeed(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) map[string]any {
	changes, err := wc.Status(ctx)
	if err != nil {
		return boardFeed{}.json()
	}
	f := a.boardFeedFor(ctx, c, wc, changedPaths(changes), true)
	a.printBoardFeed(f)
	return f.json()
}

// landFeed prints the board section after a land and returns its JSON:
// new notes for the agent, judged by the files it just landed. Claims of
// others on those files are the land's overlapping claims, which the land
// already warns about, so they are not printed again here.
func (a *App) landFeed(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, r *vxv1.LandResponse) map[string]any {
	var paths []string
	for _, fc := range r.GetChangeset().GetFiles() {
		paths = append(paths, fc.Path)
	}
	f := a.boardFeedFor(ctx, c, wc, paths, false)
	a.printBoardFeed(f)
	for _, cl := range r.OverlappingClaims {
		if cl.Trust != "self" && (f.me == "" || cl.Owner.GetRef() != f.me) {
			f.claims = append(f.claims, cl)
		}
	}
	return f.json()
}

// protoWithBoard renders a LandResponse as JSON with an added "board"
// object, keeping every field the plain response had.
func protoWithBoard(r *vxv1.LandResponse, board map[string]any) any {
	data, err := pj.Marshal(r)
	if err != nil {
		return r
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return r
	}
	out["board"] = board
	return out
}

// ---------- vx wait: decisions and interfaces on your files ----------

// noteWatch is what vx wait watches on the board: new decision and
// interface notes on the files changed in the working copy.
type noteWatch struct {
	since string
	seen  map[string]bool
	paths []string
	me    string
}

var watchedKinds = []string{"decision", "interface"}

// startNoteWatch remembers the newest decision or interface note, so only
// later ones wake vx wait. It returns nil when there is nothing to watch
// (no local changes) or the server has no board.
func (a *App) startNoteWatch(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) *noteWatch {
	changes, err := wc.Status(ctx)
	if err != nil || len(changes) == 0 {
		return nil
	}
	resp, err := c.Board.ListNotes(ctx, connect.NewRequest(&vxv1.ListNotesRequest{
		Repo: wc.Config.Repo, Effort: wc.Config.Effort, Kinds: watchedKinds, Limit: 20,
	}))
	if err != nil {
		return nil
	}
	w := &noteWatch{seen: map[string]bool{}, paths: changedPaths(changes), me: a.me(ctx, c)}
	for _, n := range resp.Msg.Notes {
		w.seen[n.Id] = true
		if after(n.CreatedAt, w.since) {
			w.since = n.CreatedAt
		}
	}
	return w
}

// waitBoardNote reports a new decision or interface note on a changed
// file for vx wait and returns true, or returns false when there is none.
func (a *App) waitBoardNote(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, w *noteWatch) bool {
	if w == nil {
		return false
	}
	resp, err := c.Board.ListNotes(ctx, connect.NewRequest(&vxv1.ListNotesRequest{
		Repo: wc.Config.Repo, Effort: wc.Config.Effort, Kinds: watchedKinds, Since: w.since, Limit: feedFetch,
	}))
	if err != nil {
		return false
	}
	var hit *vxv1.Note
	for _, n := range resp.Msg.Notes { // newest first; report the oldest new one
		if w.seen[n.Id] {
			continue
		}
		w.seen[n.Id] = true
		if n.Trust == "self" || n.Author.GetRef() == w.me || !onPaths(n.Targets, w.paths) {
			continue
		}
		hit = n
	}
	if hit == nil {
		return false
	}
	article := "A"
	if hit.Kind == "interface" {
		article = "An"
	}
	a.printf("%s %s on a file you changed arrived. %s\n", article, hit.Kind, boardFraming)
	a.printNote(hit, noteOpts{me: w.me, indent: "  "})
	a.printf("Read it before you continue; see the board with: vx board\n")
	a.emit(map[string]any{"reason": "note", "note": json.RawMessage(mustProtoJSON(hit))})
	return true
}

func mustProtoJSON(n *vxv1.Note) []byte {
	data, err := pj.Marshal(n)
	if err != nil {
		return []byte("null")
	}
	return data
}

// ---------- vx status ----------

// statusFeed reads what the Board section of vx status adds: others'
// claims on the changed files and the new notes since the board was last
// seen. It marks those notes as seen, since status shows them.
func (a *App) statusFeed(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy, me string, paths []string) ([]*vxv1.Note, bool) {
	notes, newest, ok := a.newNotes(ctx, c, wc, me, paths)
	if !ok {
		return nil, false
	}
	markBoardSeen(wc, newest)
	return notes, true
}
