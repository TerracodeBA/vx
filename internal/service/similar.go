package service

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/TerracodeBA/vx/internal/store"
)

// maxSimilarCandidates bounds how many open questions of an effort Ask
// compares a new question with (the newest ones).
const maxSimilarCandidates = 200

// similarThreshold is the share of words two questions must have in common
// (of all the words either uses, small words left out) to count as the same
// question.
const similarThreshold = 0.6

// similarQuestion returns the open question in open that the new question
// (body, targets, addressed to to) closely matches, best match first:
//
//   - targets: both have no --on targets, or they share at least one;
//   - addressee: the same, or one of the two is to anyone;
//   - words: lowercased words (letters and digits), without small words
//     such as "the" or "is", overlap by at least 60% (Jaccard); a question
//     made only of small words compares all its words.
//
// Ties go to the older question, which is the one the other asker waits on.
func similarQuestion(body string, targets []string, to *uuid.UUID, open []store.Note) (store.Note, bool) {
	words := questionWords(body)
	if len(words) == 0 {
		return store.Note{}, false
	}
	var best store.Note
	bestScore := 0.0
	for _, q := range open {
		if q.Kind != string(NoteQuestion) || q.State != NoteOpen {
			continue
		}
		if !sameTargets(targets, q.Targets) {
			continue
		}
		if to != nil && q.ToID != nil && *to != *q.ToID {
			continue
		}
		score := jaccard(words, questionWords(q.Body))
		if score < similarThreshold {
			continue
		}
		// open is newest first: >= moves the pick to the older question.
		if score >= bestScore {
			best, bestScore = q, score
		}
	}
	return best, bestScore > 0
}

func sameTargets(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// smallWords carry no meaning of their own in a question.
var smallWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the is are was were be been do does did to of in on at for by with
		from or and it its this that these those i we you he she they me us my our your what which who whom
		how when where why can could should would will shall may might must has have had not no yes if then
		than so as about any there here`) {
		smallWords[w] = true
	}
}

// questionWords returns the set of words of a question for comparison.
func questionWords(s string) map[string]bool {
	all := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := map[string]bool{}
	for _, w := range all {
		if !smallWords[w] {
			out[w] = true
		}
	}
	if len(out) == 0 {
		for _, w := range all {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	both := 0
	for w := range a {
		if b[w] {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

// SimilarNotice is what Ask tells the asker when a similar question was
// already open: "a similar open question exists: 1a2b3c4d by
// agent:alice/backend (vx board 1a2b3c4d)."
func SimilarNotice(q Note) string {
	if q.SimilarOpen == nil {
		return ""
	}
	id := shortID(q.SimilarOpen.ID)
	return fmt.Sprintf("a similar open question exists: %s by %s (vx board %s).", id, q.SimilarOpen.Author.Ref(), id)
}
