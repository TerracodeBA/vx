package object

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Origin says where a changeset came from.
type Origin string

const (
	OriginVX       Origin = "vx"       // landed by an agent or person in vx
	OriginUpstream Origin = "upstream" // imported from the git provider
)

// Author records who made a change and, for agents, on whose behalf.
type Author struct {
	Principal  string `json:"principal,omitempty"`    // e.g. "agent:alice/claude-backend"
	Name       string `json:"name,omitempty"`         // display name for git
	Email      string `json:"email,omitempty"`        // email for git
	OnBehalfOf string `json:"on_behalf_of,omitempty"` // e.g. "user:alice"
	Model      string `json:"model,omitempty"`
	Session    string `json:"session,omitempty"`
}

// Check is one piece of evidence: a command that was run and its result.
type Check struct {
	Name   string `json:"name"`
	Result string `json:"result"` // "pass" or "fail"
	Passed int    `json:"passed,omitempty"`
	Failed int    `json:"failed,omitempty"`
	// RanByVX is true when vx ran the command itself (vx land --run),
	// false when the author reported the result (vx land --check).
	RanByVX    bool  `json:"ran_by_vx,omitempty"`
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Notes qualify the result, e.g. how many Go test packages came from
	// the test cache instead of running.
	Notes string `json:"notes,omitempty"`
}

// Evidence is what an author offers as proof that a change works.
type Evidence struct {
	Checks   []Check  `json:"checks,omitempty"`
	NewTests []string `json:"new_tests,omitempty"`
}

// IsEmpty reports whether there is no evidence.
func (e Evidence) IsEmpty() bool { return len(e.Checks) == 0 && len(e.NewTests) == 0 }

// Changeset is one change: a snapshot (tree) on top of its parents, with the
// intent and provenance that git has no place for. Its hash covers every
// field, so provenance cannot be altered after the fact.
type Changeset struct {
	Version   int         `json:"v"`
	ChangeID  string      `json:"change_id"`
	Parents   []hash.Hash `json:"parents,omitempty"`
	Tree      hash.Hash   `json:"tree"`
	Origin    Origin      `json:"origin"`
	GitSHA    string      `json:"git_sha,omitempty"`
	Author    Author      `json:"author"`
	Intent    string      `json:"intent"`
	DependsOn []string    `json:"depends_on,omitempty"`
	Evidence  *Evidence   `json:"evidence,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	// SnapshotTree is the tree the author actually had, set when the server
	// merged it onto newer work so Tree differs. Evidence describes this
	// tree, not Tree.
	SnapshotTree hash.Hash `json:"snapshot_tree,omitempty"`
	// Rebase is set on a change that a rebase of its effort replayed onto a
	// newer base. Tree is the replayed tree; the evidence still describes
	// the tree of the change it was replayed from.
	Rebase *Rebase `json:"rebase,omitempty"`
}

// Rebase records how a change got onto a new base: which target commit the
// effort was rebased onto, which changeset this one replays, who ran the
// rebase and, when the replay conflicted, who resolved it.
type Rebase struct {
	Onto       hash.Hash `json:"onto"`                   // target changeset the effort was rebased onto
	OntoGitSHA string    `json:"onto_git_sha,omitempty"` // its git commit, when it came from upstream
	Branch     string    `json:"branch,omitempty"`       // the target branch, e.g. "main"
	From       hash.Hash `json:"from"`                   // the changeset this one replays
	By         string    `json:"by,omitempty"`           // principal that ran the rebase
	ResolvedBy string    `json:"resolved_by,omitempty"`  // principal that resolved a conflict on this change during the rebase
	At         time.Time `json:"at"`
}

// Rebased reports whether a rebase replayed this change onto a newer base,
// so its evidence was gathered before the rebase.
func (c *Changeset) Rebased() bool { return c.Rebase != nil }

// RebasedOnto names the target a rebased change sits on: the git commit
// when known, the changeset hash otherwise. Empty when not rebased.
func (c *Changeset) RebasedOnto() string {
	if c.Rebase == nil {
		return ""
	}
	if c.Rebase.OntoGitSHA != "" {
		return c.Rebase.OntoGitSHA
	}
	return c.Rebase.Onto.String()
}

// Merged reports whether the server merged the author's snapshot onto newer
// work, so the evidence was gathered on a different tree than the one landed.
func (c *Changeset) Merged() bool { return !c.SnapshotTree.IsZero() && c.SnapshotTree != c.Tree }

// CurrentVersion is the changeset encoding version.
const CurrentVersion = 1

// Validate checks the changeset is well formed.
func (c *Changeset) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("changeset version %d not supported", c.Version)
	}
	if !ValidChangeID(c.ChangeID) {
		return fmt.Errorf("invalid change id %q", c.ChangeID)
	}
	if c.Origin != OriginVX && c.Origin != OriginUpstream {
		return fmt.Errorf("invalid origin %q", c.Origin)
	}
	if c.Tree.IsZero() {
		return errors.New("changeset has no tree")
	}
	if strings.TrimSpace(c.Intent) == "" {
		return errors.New("changeset has no intent")
	}
	if c.CreatedAt.IsZero() {
		return errors.New("changeset has no timestamp")
	}
	if r := c.Rebase; r != nil && (r.Onto.IsZero() || r.From.IsZero() || r.At.IsZero()) {
		return errors.New("changeset rebase needs onto, from and a timestamp")
	}
	for _, p := range c.Parents {
		if p.IsZero() {
			return errors.New("changeset has a zero parent")
		}
	}
	return nil
}

// Encode returns the canonical bytes that the hash covers.
func (c *Changeset) Encode() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	cc := *c
	cc.CreatedAt = c.CreatedAt.UTC().Truncate(time.Microsecond) // Postgres precision
	if c.Rebase != nil {
		r := *c.Rebase
		r.At = r.At.UTC().Truncate(time.Microsecond)
		cc.Rebase = &r
	}
	if cc.Evidence != nil && cc.Evidence.IsEmpty() {
		cc.Evidence = nil
	}
	return json.Marshal(&cc)
}

// Hash returns the changeset's hash.
func (c *Changeset) Hash() (hash.Hash, error) {
	data, err := c.Encode()
	if err != nil {
		return hash.Zero, err
	}
	return hash.Of(hash.KindChangeset, data), nil
}

// Normalize applies the same normalization as Encode, so a changeset read
// back from the database hashes to the same value.
func (c *Changeset) Normalize() {
	c.CreatedAt = c.CreatedAt.UTC().Truncate(time.Microsecond)
	if c.Rebase != nil {
		c.Rebase.At = c.Rebase.At.UTC().Truncate(time.Microsecond)
	}
	if c.Evidence != nil && c.Evidence.IsEmpty() {
		c.Evidence = nil
	}
}

// DecodeChangeset parses canonical changeset bytes.
func DecodeChangeset(data []byte) (*Changeset, error) {
	var c Changeset
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// FirstParent returns the first parent or Zero.
func (c *Changeset) FirstParent() hash.Hash {
	if len(c.Parents) == 0 {
		return hash.Zero
	}
	return c.Parents[0]
}

// Title returns the first line of the intent.
func (c *Changeset) Title() string {
	t, _, _ := strings.Cut(strings.TrimSpace(c.Intent), "\n")
	return t
}

// Change IDs are stable across rebases. They look like "kxqr-7m2p": eight
// characters from an alphabet without look-alike letters, split for reading.
const changeIDAlphabet = "bcdfghjkmnpqrstvwxz23456789"

// NewChangeID returns a fresh random change ID.
func NewChangeID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	b := make([]byte, 0, 9)
	for i, r := range raw {
		if i == 4 {
			b = append(b, '-')
		}
		b = append(b, changeIDAlphabet[int(r)%len(changeIDAlphabet)])
	}
	return string(b)
}

// ValidChangeID reports whether s has the change ID format.
func ValidChangeID(s string) bool {
	if len(s) != 9 || s[4] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 4 {
			continue
		}
		if !strings.ContainsRune(changeIDAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

// ChangeIDFrom derives a change ID from bytes, such as a git commit hash, so
// importing the same commit twice gives the same change ID.
func ChangeIDFrom(b []byte) string {
	out := make([]byte, 0, 9)
	for i := 0; i < 8; i++ {
		if i == 4 {
			out = append(out, '-')
		}
		var v byte
		if i < len(b) {
			v = b[i]
		}
		out = append(out, changeIDAlphabet[int(v)%len(changeIDAlphabet)])
	}
	return string(out)
}
