package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	vxv1 "github.com/TerracodeBA/vx/gen/vx/v1"
)

// Organizations, teams and repository grants.

func (a *App) orgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Manage organizations and their members",
		Long: `Every repository belongs to an organization. Owners and admins of an
organization are admin on all its repositories; members get the
organization's default role (none, read or write). Everyone also has a
personal organization named like them.`,
	}

	var display string
	create := &cobra.Command{
		Use:   "create HANDLE",
		Short: "Create an organization; you become its owner",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.CreateOrg(ctxOf(cmd), connect.NewRequest(&vxv1.CreateOrgRequest{Handle: args[0], DisplayName: display}))
			if err != nil {
				return err
			}
			o := resp.Msg.Org
			a.printf("Created organization %s; you are its owner. Members get %s on its repositories.\n", o.Handle, o.DefaultRepoRole)
			a.printf("Next: vx org member %s PERSON --role member, then vx repo create %s/NAME\n", o.Handle, o.Handle)
			a.emit(resp.Msg)
			return nil
		},
	}
	create.Flags().StringVar(&display, "name", "", "display name")

	var all bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List your organizations (superadmins: --all for every one)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.ListOrgs(ctxOf(cmd), connect.NewRequest(&vxv1.ListOrgsRequest{All: all}))
			if err != nil {
				return err
			}
			for _, o := range resp.Msg.Orgs {
				role := o.MyRole
				if role == "" {
					role = "-"
				}
				kind := ""
				if o.Personal {
					kind = "personal"
				}
				a.printf("%-24s %-7s default %-6s %s\n", o.Handle, role, o.DefaultRepoRole, kind)
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "every organization on the server (superadmins)")

	show := &cobra.Command{
		Use:   "show ORG",
		Short: "Show an organization's members, teams and the repositories you can read",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.GetOrg(ctxOf(cmd), connect.NewRequest(&vxv1.GetOrgRequest{Handle: args[0]}))
			if err != nil {
				return err
			}
			m := resp.Msg
			a.printf("%s (%s)", m.Org.Handle, m.Org.DisplayName)
			if m.Org.Personal {
				a.printf(", personal")
			}
			a.printf("\nDefault role for members: %s\n", m.Org.DefaultRepoRole)
			if m.Org.MyRole != "" {
				a.printf("You are: %s\n", m.Org.MyRole)
			}
			a.printf("\nMembers (%d):\n", len(m.Members))
			for _, mem := range m.Members {
				teams := ""
				if len(mem.Teams) > 0 {
					teams = "teams: " + strings.Join(mem.Teams, ", ")
				}
				a.printf("  %-24s %-7s %s\n", mem.Person.Ref, mem.Role, teams)
			}
			if len(m.Teams) > 0 {
				a.printf("\nTeams (%d):\n", len(m.Teams))
				for _, t := range m.Teams {
					var refs []string
					for _, p := range t.Members {
						refs = append(refs, p.Handle)
					}
					a.printf("  %-24s %s\n", t.Org+"/"+t.Name, strings.Join(refs, ", "))
				}
			}
			a.printf("\nRepositories you can read (%d):\n", len(m.Repos))
			for _, r := range m.Repos {
				a.printf("  %-30s %s\n", r.Name, r.MyRole)
			}
			a.emit(m)
			return nil
		},
	}

	setDefault := &cobra.Command{
		Use:   "set-default ORG none|read|write",
		Short: "Set the role every member gets on the organization's repositories",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.UpdateOrg(ctxOf(cmd), connect.NewRequest(&vxv1.UpdateOrgRequest{Handle: args[0], DefaultRepoRole: args[1]}))
			if err != nil {
				return err
			}
			a.printf("Members of %s now get %s on its repositories.\n", resp.Msg.Org.Handle, resp.Msg.Org.DefaultRepoRole)
			a.emit(resp.Msg)
			return nil
		},
	}

	var role string
	member := &cobra.Command{
		Use:   "member ORG PERSON --role owner|admin|member",
		Short: "Add a person to an organization or change their role",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.SetMember(ctxOf(cmd), connect.NewRequest(&vxv1.SetMemberRequest{Org: args[0], Person: args[1], Role: role}))
			if err != nil {
				return err
			}
			a.printf("%s is %s of %s.\n", resp.Msg.Member.Person.Ref, article(resp.Msg.Member.Role), args[0])
			a.emit(resp.Msg)
			return nil
		},
	}
	member.Flags().StringVar(&role, "role", "member", "owner, admin or member")

	remove := &cobra.Command{
		Use:   "remove ORG PERSON",
		Short: "Remove a person from an organization and its teams",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.RemoveMember(ctxOf(cmd), connect.NewRequest(&vxv1.RemoveMemberRequest{Org: args[0], Person: args[1]}))
			if err != nil {
				return err
			}
			a.printf("Removed %s from %s and its teams.\n", args[1], args[0])
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.AddCommand(create, list, show, setDefault, member, remove, a.orgAgreementCmd(), a.orgChecksCmd())
	cmd.AddCommand(a.orgDeleteCmds()...)
	return cmd
}

func article(role string) string {
	if role == "owner" || role == "admin" {
		return "an " + role
	}
	return "a " + role
}

// splitTeam parses "ORG/TEAM".
func splitTeam(s string) (string, string, error) {
	org, team, ok := strings.Cut(strings.TrimPrefix(s, "team:"), "/")
	if !ok || org == "" || team == "" {
		return "", "", fmt.Errorf("team %q: write it as ORG/TEAM, for example acme/backend", s)
	}
	return org, team, nil
}

func (a *App) teamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "team",
		Short: "Manage an organization's teams",
		Long: `Teams group an organization's members so repositories can be granted to
all of them at once: vx repo grant REPO team:ORG/TEAM --role write.`,
	}
	printTeam := func(t *vxv1.Team) {
		var refs []string
		for _, p := range t.Members {
			refs = append(refs, p.Ref)
		}
		if len(refs) == 0 {
			a.printf("Team %s/%s has no members.\n", t.Org, t.Name)
			return
		}
		a.printf("Team %s/%s: %s\n", t.Org, t.Name, strings.Join(refs, ", "))
	}
	create := &cobra.Command{
		Use:   "create ORG/TEAM",
		Short: "Create a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, team, err := splitTeam(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.CreateTeam(ctxOf(cmd), connect.NewRequest(&vxv1.CreateTeamRequest{Org: org, Name: team}))
			if err != nil {
				return err
			}
			a.printf("Created team %s/%s. Add members: vx team add %s/%s PERSON\n", org, resp.Msg.Team.Name, org, resp.Msg.Team.Name)
			a.emit(resp.Msg)
			return nil
		},
	}
	del := &cobra.Command{
		Use:   "delete ORG/TEAM",
		Short: "Delete a team and its repository grants",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, team, err := splitTeam(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.DeleteTeam(ctxOf(cmd), connect.NewRequest(&vxv1.DeleteTeamRequest{Org: org, Name: team}))
			if err != nil {
				return err
			}
			a.printf("Deleted team %s/%s and its repository grants.\n", org, team)
			a.emit(resp.Msg)
			return nil
		},
	}
	add := &cobra.Command{
		Use:   "add ORG/TEAM PERSON",
		Short: "Add a member of the organization to a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, team, err := splitTeam(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.SetTeamMember(ctxOf(cmd), connect.NewRequest(&vxv1.SetTeamMemberRequest{Org: org, Team: team, Person: args[1]}))
			if err != nil {
				return err
			}
			printTeam(resp.Msg.Team)
			a.emit(resp.Msg)
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove ORG/TEAM PERSON",
		Short: "Remove a person from a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, team, err := splitTeam(args[0])
			if err != nil {
				return err
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.RemoveTeamMember(ctxOf(cmd), connect.NewRequest(&vxv1.RemoveTeamMemberRequest{Org: org, Team: team, Person: args[1]}))
			if err != nil {
				return err
			}
			printTeam(resp.Msg.Team)
			a.emit(resp.Msg)
			return nil
		},
	}
	cmd.AddCommand(create, del, add, remove)
	return cmd
}

// repoAccessCmds are vx repo grant, revoke and access.
func (a *App) repoAccessCmds() []*cobra.Command {
	var role string
	grant := &cobra.Command{
		Use:   "grant REPO SUBJECT --role read|write|admin",
		Short: "Give a person (alice, user:alice), a service (service:ci) or a team (team:ORG/TEAM) a role on a repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return fmt.Errorf("which role? pass --role read, write or admin")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.GrantRepo(ctxOf(cmd), connect.NewRequest(&vxv1.GrantRepoRequest{Repo: args[0], Subject: args[1], Role: role}))
			if err != nil {
				return err
			}
			g := resp.Msg.Grant
			a.printf("Granted %s %s on %s.\n", g.Subject, g.Role, g.Repo)
			a.emit(resp.Msg)
			return nil
		},
	}
	grant.Flags().StringVar(&role, "role", "", "read, write or admin")

	revoke := &cobra.Command{
		Use:   "revoke REPO SUBJECT",
		Short: "Remove a direct grant from a repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.RevokeRepo(ctxOf(cmd), connect.NewRequest(&vxv1.RevokeRepoRequest{Repo: args[0], Subject: args[1]}))
			if err != nil {
				return err
			}
			a.printf("Revoked the grant of %s on %s. They may still have access through the organization; vx repo access %s shows how.\n", args[1], args[0], args[0])
			a.emit(resp.Msg)
			return nil
		},
	}

	access := &cobra.Command{
		Use:   "access REPO",
		Short: "Show a repository's grants and everyone's role on it, and why (repository admins)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			resp, err := c.Orgs.RepoAccess(ctxOf(cmd), connect.NewRequest(&vxv1.RepoAccessRequest{Repo: args[0]}))
			if err != nil {
				return err
			}
			a.printf("Direct grants on %s (%d):\n", args[0], len(resp.Msg.Grants))
			for _, g := range resp.Msg.Grants {
				a.printf("  %-30s %s\n", g.Subject, g.Role)
			}
			a.printf("\nEffective roles (%d):\n", len(resp.Msg.People))
			for _, p := range resp.Msg.People {
				a.printf("  %-30s %-6s via %s\n", p.Person.Ref, p.Role, strings.Join(p.Via, ", "))
			}
			a.emit(resp.Msg)
			return nil
		},
	}
	return []*cobra.Command{grant, revoke, access, a.supportCmd()}
}
