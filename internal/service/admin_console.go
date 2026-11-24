package service

// The server console (vx-server): commands run by whoever operates the
// server, with database access and no signed-in actor. Op log entries
// they write have no actor.

import (
	"context"
	"errors"
	"strings"

	"github.com/TerracodeBA/vx/internal/store"
)

// ConsoleSetSuperadmin makes the person handle a superadmin (on) or not,
// for vx-server superadmin grant|revoke. changed is false when they already
// were (or were not) one. Agents and service principals cannot be
// superadmins.
func (s *Service) ConsoleSetSuperadmin(ctx context.Context, handle string, on bool) (p store.Principal, changed bool, err error) {
	handle = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(handle)), "user:")
	if handle == "" {
		return store.Principal{}, false, errf(CodeInvalid, "which person? Give their handle, for example vx-server superadmin grant alice")
	}
	err = s.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		p, err = tx.PrincipalByHandle(ctx, handle)
		if errors.Is(err, store.ErrNotFound) {
			return errf(CodeNotFound, "there is no person %s on this server; vx-server user-add %s creates one, or they sign in once first", handle, handle)
		}
		if err != nil {
			return err
		}
		if p.Kind != store.KindHuman {
			return errf(CodeInvalid, "%s is a %s, and only people can be superadmins", p.Ref(), p.Kind)
		}
		if p.SiteAdmin == on {
			return nil
		}
		if err := tx.SetSiteAdmin(ctx, p.ID, on); err != nil {
			return err
		}
		changed = true
		op := "person.superadmin_grant"
		if !on {
			op = "person.superadmin_revoke"
		}
		p.SiteAdmin = on
		return tx.LogOp(ctx, nil, nil, op, p.Ref(), map[string]any{"superadmin": !on}, map[string]any{"superadmin": on, "via": "console"})
	})
	return p, changed, classify(err)
}

// ConsoleSuperadmins lists the superadmins, by handle (vx-server
// superadmin list).
func (s *Service) ConsoleSuperadmins(ctx context.Context) ([]store.Principal, error) {
	people, err := s.Store.ListPeople(ctx)
	if err != nil {
		return nil, classify(err)
	}
	var out []store.Principal
	for _, p := range people {
		if p.SiteAdmin {
			out = append(out, p)
		}
	}
	return out, nil
}
