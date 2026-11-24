// Package provider holds the types shared by the service layer and the git
// bridge for shipping changes to a git host.
package provider

import (
	"errors"
	"time"

	"github.com/TerracodeBA/vx/internal/hash"
)

// Commit is one commit to write: a vx tree and its metadata.
type Commit struct {
	Tree        hash.Hash
	Message     string
	AuthorName  string
	AuthorEmail string
	When        time.Time
}

// PullRequest is what vx asks the provider to open.
type PullRequest struct {
	Head  string // branch with the changes, e.g. "vx/rate-limiting"
	Base  string // target branch
	Title string
	Body  string
	Draft bool
}

// OpenedPR is the provider's answer.
type OpenedPR struct {
	Number  int
	URL     string
	Updated bool // an open PR for the branch existed and was updated
}

// ErrNoPullRequests means the provider cannot open pull requests (plain git).
var ErrNoPullRequests = errors.New("this provider has no pull requests")
