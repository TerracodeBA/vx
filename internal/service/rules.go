package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/TerracodeBA/vx/internal/engine"
	"github.com/TerracodeBA/vx/internal/hash"
	"github.com/TerracodeBA/vx/internal/object"
	"github.com/TerracodeBA/vx/internal/rules"
)

// mergeOptions are the options of a merge whose sides are trees (see
// rules.MergeOptions).
func (s *Service) mergeOptions(ctx context.Context, trees ...hash.Hash) []engine.MergeOption {
	return rules.MergeOptions(ctx, s.Blobs, trees...)
}

// ruleWarnings checks tree against the vx.toml in it, for the files
// changed since base: [[unique]] violations, and a vx.toml that this change
// broke. They only warn, like the Go checks.
func (s *Service) ruleWarnings(ctx context.Context, base, tree hash.Hash) []engine.Warning {
	r, err := rules.At(ctx, s.Blobs, tree)
	if err != nil {
		if !s.rulesFileChanged(ctx, base, tree) {
			return nil // broken before this change; reported when it broke
		}
		return []engine.Warning{{Path: rules.File, Kind: rules.WarningRule, Name: rules.File,
			Detail: fmt.Sprintf("vx.toml does not parse, so its rules are not applied: %v", err)}}
	}
	ws, err := rules.Check(ctx, s.Blobs, r, base, tree)
	if err != nil {
		slog.Warn("vx.toml rules could not be checked", "tree", tree.Short(), "err", err)
		return []engine.Warning{{Kind: engine.WarningCheck, Detail: fmt.Sprintf("could not check the vx.toml rules: %v", err)}}
	}
	return ws
}

// withRequiredChecks returns the evidence to store for a snapshot: ev with
// NotVerified set to the vx.toml [checks] required commands (rules at
// tree, the snapshot the evidence describes) that vx did not run. A
// client's NotVerified is never kept. ev itself is not modified.
func (s *Service) withRequiredChecks(ctx context.Context, tree hash.Hash, ev *object.Evidence) *object.Evidence {
	var missing []string
	if r, err := rules.At(ctx, s.Blobs, tree); err == nil && len(r.RequiredChecks) > 0 {
		var ran []string
		if ev != nil {
			for _, c := range ev.Checks {
				if c.RanByVX {
					ran = append(ran, c.Name)
				}
			}
		}
		missing = rules.MissingChecks(r, ran)
	}
	if ev == nil && len(missing) == 0 {
		return nil
	}
	out := &object.Evidence{}
	if ev != nil {
		*out = *ev
	}
	out.NotVerified = missing
	if out.IsEmpty() {
		return nil
	}
	return out
}

// rulesFileChanged reports whether vx.toml differs between two trees.
func (s *Service) rulesFileChanged(ctx context.Context, a, b hash.Hash) bool {
	if a.IsZero() {
		return true
	}
	ea, oka, err := object.Lookup(ctx, s.Blobs, a, rules.File)
	if err != nil {
		return true
	}
	eb, okb, err := object.Lookup(ctx, s.Blobs, b, rules.File)
	if err != nil {
		return true
	}
	return oka != okb || ea.Hash != eb.Hash
}
