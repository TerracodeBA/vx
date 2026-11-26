package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// Who may report a check's result (#47). A check's reporter rule names who
// may: the trust rules below are pure functions of the change's author and
// the reporter, so the land and ship gates, the queue and the tests apply
// exactly the same rules. A result from a reporter the check does not allow
// is stored, but does not count, and says why.
//
//   - self: anyone who may report on the repository, the change's own
//     author (the agent or person who made it) included.
//   - independent: not the change's author and not its person or another
//     agent of the same person, unless the check says allow_same_person
//     (then only the author itself is refused). People of other teams, their
//     agents and CI tokens are independent.
//   - ci: only a CI token of the repository (vx repo ci-token create).
//   - human: only a person, never an agent or a CI token, and not the
//     person who made the change by hand (their own agents' changes they
//     may sign off, as with landing policies).

// Reporter rules.
const (
	ReporterSelf        = "self"
	ReporterIndependent = "independent"
	ReporterCI          = "ci"
	ReporterHuman       = "human"
)

// ReporterRules lists the reporter rules a check may name.
var ReporterRules = []string{ReporterSelf, ReporterIndependent, ReporterCI, ReporterHuman}

// ReporterKind is what kind of principal reports a result.
type ReporterKind string

const (
	ReporterIsPerson ReporterKind = "person"
	ReporterIsAgent  ReporterKind = "agent"
	ReporterIsCI     ReporterKind = "ci"
	// ReporterIsService is a service principal that is not a CI token.
	ReporterIsService ReporterKind = "service"
)

// ChangeAuthor is who made a change, as recorded on its changeset.
type ChangeAuthor struct {
	Principal string // "agent:alice/backend" or "user:alice"
	Person    string // "user:alice": the person behind it (the principal itself for a person)
}

// ReporterIdentity is who reports a result.
type ReporterIdentity struct {
	Kind      ReporterKind
	Principal string // ref of the reporter
	Person    string // ref of the person behind it; empty for CI and services
}

// authorOf reads a changeset's author into a ChangeAuthor.
func authorOf(principal, onBehalfOf string) ChangeAuthor {
	a := ChangeAuthor{Principal: principal, Person: onBehalfOf}
	if a.Person == "" && strings.HasPrefix(principal, "user:") {
		a.Person = principal
	}
	return a
}

// ReporterAllowed reports whether by may report a result of a check with
// reporter rule rule on a change made by author, and when not, why, in a
// sentence that says who may. An empty rule is "self".
func ReporterAllowed(rule string, allowSamePerson bool, author ChangeAuthor, by ReporterIdentity) (bool, string) {
	switch rule {
	case "", ReporterSelf:
		return true, ""
	case ReporterCI:
		if by.Kind == ReporterIsCI {
			return true, ""
		}
		return false, fmt.Sprintf("only CI may report this check (reporter = \"ci\"), with a CI token of the repository; %s is %s", by.Principal, kindPhrase(by.Kind))
	case ReporterHuman:
		if by.Kind != ReporterIsPerson {
			return false, fmt.Sprintf("only a person may report this check (reporter = \"human\"), in the web UI or with their own vx; %s is %s", by.Principal, kindPhrase(by.Kind))
		}
		if by.Principal == author.Principal {
			return false, fmt.Sprintf("%s made this change and cannot sign it off; another person must (reporter = \"human\")", by.Principal)
		}
		return true, ""
	case ReporterIndependent:
		if by.Principal != "" && by.Principal == author.Principal {
			return false, fmt.Sprintf("%s made this change; an independent reporter must (reporter = \"independent\"): not the author and not another agent of the same person", by.Principal)
		}
		if allowSamePerson || by.Kind == ReporterIsCI || by.Kind == ReporterIsService {
			return true, ""
		}
		if by.Person != "" && by.Person == author.Person {
			what := "another agent of " + author.Person
			if by.Kind == ReporterIsPerson {
				what = "the person whose agent made it"
			}
			return false, fmt.Sprintf("%s is %s; an independent reporter must (reporter = \"independent\"): someone else, their agent or CI, unless the check says allow_same_person = true", by.Principal, what)
		}
		return true, ""
	}
	return false, fmt.Sprintf("the check names an unknown reporter rule %q; use one of %s", rule, strings.Join(ReporterRules, ", "))
}

func kindPhrase(k ReporterKind) string {
	switch k {
	case ReporterIsAgent:
		return "an agent"
	case ReporterIsCI:
		return "a CI token"
	case ReporterIsService:
		return "a service account"
	}
	return "a person"
}

// reporterIdentity says who actor is as a reporter: a person, an agent
// (with its person), a CI token or another service principal.
func (s *Service) reporterIdentity(ctx context.Context, actor Actor) (ReporterIdentity, error) {
	id := ReporterIdentity{Principal: actor.Ref()}
	switch actor.Kind {
	case store.KindHuman:
		id.Kind, id.Person = ReporterIsPerson, actor.Ref()
	case store.KindAgent:
		id.Kind = ReporterIsAgent
		if actor.Parent != nil {
			id.Person = actor.Parent.Ref()
		}
	default:
		id.Kind = ReporterIsService
		if _, err := s.Store.CITokenOf(ctx, actor.ID); err == nil {
			id.Kind = ReporterIsCI
		} else if !errors.Is(err, store.ErrNotFound) {
			return id, err
		}
	}
	return id, nil
}

// ReporterCounts is the hook the result store calls when a result is
// reported: whether actor's result of a check (its reporter rule and
// allow_same_person) on a change made by author counts, and when not, why.
// The result is stored either way.
func (s *Service) ReporterCounts(ctx context.Context, actor Actor, rule string, allowSamePerson bool, author ChangeAuthor) (bool, string, error) {
	by, err := s.reporterIdentity(ctx, actor)
	if err != nil {
		return false, "", err
	}
	ok, why := ReporterAllowed(rule, allowSamePerson, author, by)
	return ok, why, nil
}
