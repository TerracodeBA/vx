package cli

// The board feed: notes and claims brought to the agent in the commands it
// already runs (vx sync, land, status, wait), so nobody has to poll vx
// board. A working copy remembers when it was last shown the board
// (Config.BoardSeenAt), and the server what each principal was shown
// (Note.read), so each note is shown once: a note vx board, vx context or
// vx wait already showed does not come back in the feed.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
	"github.com/TerracodeBA/vx/internal/client"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/workcopy"
)

const (
	// feedShown is how many new notes sync and land print (one line
	// each, but for questions to you and your own person's notes).
	feedShown = 10
	// feedFetch is how many new notes one look at the board reads.
	feedFetch = 100
	// contextBatch is the most targets one Context request takes.
	contextBatch = 20
)

// boardFeed is what the board has for a working copy right now.
type boardFeed struct {
	notes  []*vxv1.Note  // new notes for the agent, newest first; other efforts' among them
	claims []*vxv1.Claim // active claims of others on the agent's files
	newest string        // created_at of the newest note read, to mark as seen
	me     string        // the caller's ref, shown as "you"
	repo   string        // the working copy's repository
	effort string        // the working copy's effort; notes of others are marked with theirs
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
// one it was not shown yet that is a decision or interface anywhere in the
// effort, a question to the agent, or any note on one of paths.
func forMe(n *vxv1.Note, me string, paths []string) bool {
	if n.Read || n.Trust == "self" || (me != "" && n.Author.GetRef() == me) {
		return false
	}
	if n.Kind == "decision" || n.Kind == "interface" || n.Kind == "rebase" {
		return true // rebase: vx moved the effort, or says it needs moving
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
	pc, ok := a.contextFor(ctx, c, repo, slug, me, paths)
	return pc.claims, pc.notes, ok
}

// pathsContext is what Context says about some paths, over as many
// requests as the paths need.
type pathsContext struct {
	claims []*vxv1.Claim       // others' active claims in the effort
	notes  []*vxv1.Note        // the effort's notes on the paths
	others []*vxv1.OtherEffort // other open efforts on the paths, one per effort
}

// contextFor asks Context about paths, contextBatch at a time, and merges
// the answers. ok is false when a request failed.
func (a *App) contextFor(ctx context.Context, c *client.Client, repo, slug, me string, paths []string) (pathsContext, bool) {
	var pc pathsContext
	have := map[string]bool{}
	haveNote := map[string]bool{}
	other := map[string]*vxv1.OtherEffort{}
	for start := 0; start < len(paths); start += contextBatch {
		end := min(start+contextBatch, len(paths))
		resp, err := c.Board.Context(ctx, connect.NewRequest(&vxv1.ContextRequest{Repo: repo, Effort: slug, Targets: paths[start:end]}))
		if err != nil {
			return pc, false
		}
		for _, cl := range resp.Msg.Claims {
			if have[cl.Id] || cl.Trust == "self" || (me != "" && cl.Owner.GetRef() == me) {
				continue
			}
			have[cl.Id] = true
			pc.claims = append(pc.claims, cl)
		}
		for _, n := range resp.Msg.Notes {
			if !haveNote[n.Id] {
				haveNote[n.Id] = true
				pc.notes = append(pc.notes, n)
			}
		}
		for _, o := range resp.Msg.OtherEfforts {
			m, ok := other[o.Effort]
			if !ok {
				other[o.Effort] = o
				pc.others = append(pc.others, o)
				for _, cl := range o.Claims {
					have[cl.Id] = true
				}
				for _, n := range o.Notes {
					haveNote[n.Id] = true
				}
				continue
			}
			m.ChangedPaths = append(m.ChangedPaths, o.ChangedPaths...)
			for _, cl := range o.Claims {
				if !have[cl.Id] {
					have[cl.Id] = true
					m.Claims = append(m.Claims, cl)
				}
			}
			for _, n := range o.Notes {
				if !haveNote[n.Id] {
					haveNote[n.Id] = true
					m.Notes = append(m.Notes, n)
				}
			}
		}
	}
	return pc, true
}

// otherNotes are the interface and decision notes of other efforts on
// paths that the caller has not read and did not write, for the sync and
// land feed. Each carries its effort (Note.effort), which the feed prints.
func otherNotes(others []*vxv1.OtherEffort, me string) []*vxv1.Note {
	var out []*vxv1.Note
	for _, o := range others {
		for _, n := range o.Notes {
			if !n.Read && n.Trust != "self" && (me == "" || n.Author.GetRef() != me) {
				out = append(out, n)
			}
		}
	}
	return out
}

// newestFirst sorts notes by creation time, newest first.
func newestFirst(notes []*vxv1.Note) {
	sort.SliceStable(notes, func(i, j int) bool { return after(notes[i].CreatedAt, notes[j].CreatedAt) })
}

// otherEffortsJSON is the "other_efforts" list of vx status --json.
func otherEffortsJSON(others []*vxv1.OtherEffort) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(others))
	for _, o := range others {
		if data, err := pj.Marshal(o); err == nil {
			out = append(out, data)
		}
	}
	return out
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
	f := boardFeed{me: me, repo: wc.Config.Repo, effort: wc.Config.Effort}
	notes, newest, ok := a.newNotes(ctx, c, wc, me, paths)
	if !ok {
		return f
	}
	f.notes, f.newest = notes, newest
	if len(paths) > 0 {
		// One look at Context gives others' claims on the paths and other
		// efforts' new interfaces and decisions on them.
		if pc, ok := a.contextFor(ctx, c, wc.Config.Repo, wc.Config.Effort, me, paths); ok {
			if withClaims {
				f.claims = pc.claims
			}
			if more := otherNotes(pc.others, me); len(more) > 0 {
				f.notes = append(f.notes, more...)
				newestFirst(f.notes)
			}
		}
	}
	markBoardSeen(wc, newest)
	return f
}

// printBoardFeed prints the board section that ends vx sync and vx land:
// new notes for the agent, one line each, then others' claims on its
// files. Questions to the agent and notes of its own person print in full;
// the line after the list says how to read the rest (vx board ID). It
// prints nothing when there is nothing new. Every note it prints counts as
// read.
func (a *App) printBoardFeed(ctx context.Context, c *client.Client, f boardFeed) {
	me := f.me
	p := &notePrinter{a: a, repo: f.repo, effort: f.effort, full: func(n *vxv1.Note) bool { return fullInFeed(n, me) }}
	defer func() { a.markRead(ctx, c, p.shown...) }()
	if len(f.notes) > 0 {
		a.printf("\nNew on the board since your last sync (%d). %s\n", len(f.notes), boardFraming)
		if n := f.fromOthers(); n > 0 {
			a.printf("  %d from other efforts on your files (marked \"in EFFORT\"): they ship separately, so your sync does not bring their changes.\n", n)
		}
		roots, replies := threads(f.notes)
		shown := 0
		for _, n := range roots {
			if shown == feedShown {
				break
			}
			p.print(n, noteOpts{me: me, indent: "  ", where: f.otherEffort(n)})
			shown++
			for _, r := range replies[n.Id] {
				if shown == feedShown {
					break
				}
				p.print(r, noteOpts{me: me, indent: "    "})
				shown++
			}
		}
		if more := len(f.notes) - shown; more > 0 {
			a.printf("  %d more: vx board\n", more)
		}
		p.readHint("  ")
	}
	if len(f.claims) > 0 {
		a.printf("\nClaimed by others (%d): they are changing these now; coordinate before you land.\n", len(f.claims))
		for _, cl := range f.claims {
			a.printf("  %s  id %s\n", claimLine(cl), noteID(cl.Id))
		}
	}
}

// otherEffort reports whether n is another effort's note.
func (f boardFeed) otherEffort(n *vxv1.Note) bool {
	return n.Effort != "" && f.effort != "" && n.Effort != f.effort
}

// fromOthers counts the feed's notes of other efforts.
func (f boardFeed) fromOthers() int {
	k := 0
	for _, n := range f.notes {
		if f.otherEffort(n) {
			k++
		}
	}
	return k
}

// printClaim prints a claim like a note: trust label, owner, targets,
// expiry, then the reason under a gutter.
func (a *App) printClaim(cl *vxv1.Claim, indent string) {
	trust := trustLabel(cl.Trust)
	var b strings.Builder
	b.WriteString("[" + trust + "] claim  " + cl.Owner.GetRef())
	if len(cl.Targets) > 0 {
		b.WriteString("  on " + strings.Join(cl.Targets, ", "))
	}
	if cl.WorkspaceId != "" {
		b.WriteString("  while its workspace is open")
	} else if cl.ExpiresAt != "" {
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
	trust := trustLabel(cl.Trust)
	s := "[" + trust + "] " + cl.Owner.GetRef() + " on " + longTargets(cl.Targets)
	if cl.WorkspaceId != "" {
		s += " while its workspace is open"
	} else if cl.ExpiresAt != "" {
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
	a.reportChanges(ctx, c, wc, changes)
	f := a.boardFeedFor(ctx, c, wc, changedPaths(changes), true)
	a.printBoardFeed(ctx, c, f)
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
	a.printBoardFeed(ctx, c, f)
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

// watchedKinds wake vx wait: decisions and interfaces on the agent's
// files, review comments and CI results addressed to it, and rebase notes.
var watchedKinds = []string{"decision", "interface", "review", "ci", "rebase"}

// startNoteWatch remembers the newest watched note, so only later ones
// wake vx wait. It returns nil when the server has no board.
func (a *App) startNoteWatch(ctx context.Context, c *client.Client, wc *workcopy.WorkingCopy) *noteWatch {
	changes, err := wc.Status(ctx)
	if err != nil {
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
		if n.Read || n.Trust == "self" || n.Author.GetRef() == w.me {
			continue
		}
		switch {
		case feedbackKinds[n.Kind] && w.me != "" && n.To == w.me:
		case n.Kind == "rebase":
		case (n.Kind == "decision" || n.Kind == "interface") && onPaths(n.Targets, w.paths):
		default:
			continue
		}
		hit = n
	}
	if hit == nil {
		return false
	}
	switch hit.Kind {
	case "review":
		a.printf("A review comment for you arrived from the git host. %s\n", boardFraming)
	case "ci":
		a.printf("A CI result for your changes arrived from the git host. %s\n", boardFraming)
	case "rebase":
		a.printf("vx rebased this effort, or says it needs a rebase. %s\n", boardFraming)
	case "interface":
		a.printf("An interface on a file you changed arrived. %s\n", boardFraming)
	default:
		a.printf("A %s on a file you changed arrived. %s\n", hit.Kind, boardFraming)
	}
	a.printNote(hit, noteOpts{me: w.me, indent: "  "})
	a.markRead(ctx, c, hit)
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

// ---------- one line per note ----------

// noteBodyShown is how much of a note's body its one-line form keeps.
const noteBodyShown = 80

// fullInFeed reports whether a feed prints n in full rather than on one
// line: a note addressed to the caller (a question, a review comment or CI
// result), or a note of the caller's own person (that person, or another
// of their agents). Everything else is one line; vx board ID prints it in
// full.
func fullInFeed(n *vxv1.Note, me string) bool {
	if me != "" && n.To == me {
		return true
	}
	return trustOf(n) == "same person"
}

// shortAuthor is the author as a one-line note names it: "backend" for
// the caller or another agent of the caller's person, "alice/backend" for someone
// else's agent, "alice" for a person. System notes name no author: their
// [vx] label says who wrote them.
func shortAuthor(n *vxv1.Note) string {
	if isSystem(n) {
		return ""
	}
	ref := n.Author.GetRef()
	kind, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return ref
	}
	if t := trustOf(n); kind == "agent" && (t == "same person" || t == "self") {
		if _, name, ok := strings.Cut(rest, "/"); ok {
			return name
		}
	}
	return rest
}

// shortTargets is a note's targets in a few words: the first one (a file
// by its base name), and how many more: "cmd_land.go (+3 more)".
func shortTargets(targets []string) string {
	if len(targets) == 0 {
		return ""
	}
	first := targets[0]
	if !strings.HasSuffix(first, "/") {
		if i := strings.LastIndex(first, "/"); i >= 0 && i < len(first)-1 {
			first = first[i+1:]
		}
	}
	if len(targets) > 1 {
		first += fmt.Sprintf(" (+%d more)", len(targets)-1)
	}
	return oneLine(cleanBody(first))
}

// longTargets lists up to three targets and counts the rest: "a.go, b.go,
// c.go and 8 more".
func longTargets(targets []string) string {
	const shown = 3
	if len(targets) <= shown+1 {
		return strings.Join(targets, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(targets[:shown], ", "), len(targets)-shown)
}

// shortAgo is agoRFC for one-line notes: "just now", "4m ago", "3h ago",
// "2d ago".
func shortAgo(s string) string {
	t, ok := parseTime(s)
	if !ok {
		return s
	}
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2 Jan 2006")
}

// clipBody is a note body on one line, cut to noteBodyShown characters;
// cut says something was left out.
func clipBody(body string) (line string, cut bool) {
	line = oneLine(cleanBody(body))
	r := []rune(line)
	if len(r) <= noteBodyShown {
		return line, false
	}
	return strings.TrimRight(string(r[:noteBodyShown-1]), " ") + "…", true
}

// noteLine is a note on one line:
//
//	[same person] interface px-speed on cmd_land.go (+3 more) 2m ago  id 1a2b3c4d: first 80 characters…
//
// cut says the body was shortened, so vx board ID has more.
func noteLine(n *vxv1.Note, o noteOpts) (line string, cut bool) {
	var b strings.Builder
	b.WriteString("[" + trustOf(n) + "] " + n.Kind)
	if who := shortAuthor(n); who != "" {
		b.WriteString(" " + who)
	}
	if n.Kind == "question" {
		to := "anyone"
		switch {
		case n.To != "" && n.To == o.me:
			to = "you"
		case n.To != "":
			to = n.To
			if _, rest, ok := strings.Cut(to, ":"); ok {
				to = rest
			}
		}
		b.WriteString(" -> " + to)
	}
	if t := shortTargets(n.Targets); t != "" {
		b.WriteString(" on " + t)
	}
	if o.where && n.Effort != "" {
		b.WriteString(" in " + n.Effort)
	}
	b.WriteString(" " + shortAgo(n.CreatedAt))
	if n.Kind == "question" && n.State != "" && n.State != "open" {
		b.WriteString(" " + n.State)
	}
	if n.Kind == "answer" && n.ThreadId != "" {
		b.WriteString(" re " + noteID(n.ThreadId))
	}
	if n.SupersededBy != "" {
		b.WriteString(" replaced by " + noteID(n.SupersededBy))
	}
	b.WriteString("  id " + noteID(n.Id))
	body, cut := clipBody(n.Body)
	if body != "" {
		b.WriteString(": " + body)
	}
	return b.String(), cut
}

// notePrinter prints notes the quiet way: in full the ones full says,
// every other one on one line, and remembers the one-line notes whose body
// it cut, for the "Read in full" hint.
type notePrinter struct {
	a            *App
	full         func(*vxv1.Note) bool
	repo, effort string       // where vx board looks without --repo and --effort
	cut          []*vxv1.Note // one-line notes whose body was cut
	shown        []*vxv1.Note // every note printed
	inFull       []*vxv1.Note // the notes printed in full
}

func (p *notePrinter) print(n *vxv1.Note, o noteOpts) {
	p.shown = append(p.shown, n)
	if p.full != nil && p.full(n) {
		p.inFull = append(p.inFull, n)
		p.a.printNote(n, o)
		return
	}
	line, cut := noteLine(n, o)
	p.a.printf("%s%s\n", o.indent, line)
	if cut {
		p.cut = append(p.cut, n)
	}
}

// readHint prints one line saying how to read the cut notes in full:
// "Read in full: vx board 1a2b3c4d 5e6f7a8b", with --effort (and --repo)
// for notes of other efforts. It prints nothing when no body was cut.
func (p *notePrinter) readHint(indent string) {
	if len(p.cut) == 0 {
		return
	}
	type place struct{ repo, effort string }
	var order []place
	ids := map[place][]string{}
	for _, n := range p.cut {
		var pl place
		if n.Repo != "" && n.Repo != p.repo {
			pl = place{repo: n.Repo, effort: n.Effort}
		} else if n.Effort != "" && n.Effort != p.effort {
			pl.effort = n.Effort
		}
		if _, ok := ids[pl]; !ok {
			order = append(order, pl)
		}
		if id := noteID(n.Id); !contains(ids[pl], id) {
			ids[pl] = append(ids[pl], id)
		}
	}
	var cmds []string
	for _, pl := range order {
		cmd := "vx board " + strings.Join(ids[pl], " ")
		if pl.effort != "" {
			cmd += " --effort " + pl.effort
		}
		if pl.repo != "" {
			cmd += " --repo " + pl.repo
		}
		cmds = append(cmds, cmd)
	}
	p.a.printf("%sRead in full: %s\n", indent, oneLine(cleanBody(strings.Join(cmds, "; "))))
}

// isSystem reports whether vx itself posted n (overlaps, rebases, review
// comments and CI results from the git host).
func isSystem(n *vxv1.Note) bool {
	return n.System || n.Trust == "vx"
}
