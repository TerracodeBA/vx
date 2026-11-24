# vx for organizations

vx has organizations, teams and repository roles. People see and change only the repositories they have a role on, and agents act with exactly the role of their person. This page describes what exists today, how to set it up, and what is still planned.

## Three ways to run vx

| | Individual | Team | Organization |
|---|---|---|---|
| Who | One person and their agents | A small group that trusts each other | Many teams, roles, audits |
| Where | Laptop or workstation | One shared server | Several replicas behind a load balancer |
| Sign-in | Token | Token per person | Single sign-on (OIDC, SAML) |
| Access | Everything | Per organization, team and repository | Per organization, team and repository |
| Agents | Your own | Each person's own, with their person's role | Scoped to paths, with budgets |
| GitHub | SSH or personal token | Shared token | GitHub App per organization |
| Storage | Local disk | Server disk | Object storage (S3, R2, GCS) |
| Status | Ready | Ready | Access control ready; SSO, scale-out and the GitHub App planned |

## Organizations

Every repository belongs to one organization.

- **Personal organizations.** Every person has one, named like them (`alice`), with only them in it. A repository you create without naming an organization goes there unless you own or administer the organization named like its owner part.
- **Shared organizations.** Any person can create one with `vx org create acme` and becomes its owner.
- **Members.** A member has a role in the organization: `owner`, `admin` or `member`.
  - Owners and admins are admin on every repository of the organization, change its members and teams, and create repositories in it.
  - Only owners make, demote or remove owners. The last owner cannot be removed or demoted.
  - Members get the organization's **default repository role** on all its repositories: `read` (the default for new organizations), `write` or `none`.
  - Anyone may leave an organization (`vx org remove ORG THEMSELVES`), except its last owner.
- **Teams** group an organization's members (`acme/backend`) so a repository can be granted to all of them at once. Removing someone from the organization removes them from its teams.
- **Site admins** are admin on every repository and see every organization and person. They create people from the web UI or API. `vx-server user-add --admin` makes one.

## Repository roles

A repository role is `read`, `write` or `admin`; each includes the one before it. Your role on a repository is the highest of:

1. a direct grant to you (`vx repo grant acme/api alice --role write`),
2. a grant to a team you are in (`vx repo grant acme/api team:acme/backend --role write`),
3. the organization's default role, if you are a member,
4. admin, if you are an owner or admin of the organization,
5. admin, if you are a site admin.

On a GitHub-backed repository none of these but the last counts: your role is your GitHub permission (see below).

**Agents** have exactly their person's role. **Service principals** (`service:ci`) have only their direct grants. Grants go to people (`alice` or `user:alice`), service principals (`service:ci`) and teams of the repository's own organization (`team:acme/backend`), never to agents.

A repository you cannot read is reported exactly like one that does not exist. One you can read but need more on gives a permission error that names the role needed and who can grant it.

Only people manage organizations, teams and grants. Agents are refused, like token creation.

## GitHub-backed repositories

A repository linked to a GitHub repository through an installation of the vx GitHub App is **GitHub-backed**: GitHub decides who may do what in it, with the rules the organization already set on GitHub (base permissions, teams, outside collaborators). vx never shows more than GitHub allows. `vx repo list` and the web UI say "roles from GitHub" for these repositories, and the API returns `github_backed: true`.

| On GitHub | In vx |
|---|---|
| admin, maintain | admin (ship, see access) |
| write | write (start efforts, land, post) |
| triage, read | read (see efforts, log, board) |
| none | no access: the repository is "not found" |
| Organization admin | admin of the linked vx organization (an owner stays owner) |
| Organization member | member of the linked vx organization |
| Not a member | not in the linked vx organization |

- **Who has a role.** People linked to GitHub (they signed in with GitHub). A person without a GitHub link has no role on GitHub-backed repositories. Site admins keep admin everywhere. Agents have exactly their person's role. Service principals (`service:ci`) have only their direct vx grants, since GitHub does not know them; repository admins may grant them.
- **Your account or organization.** You get a role on a repository of an installation on your own GitHub account, or of an organization GitHub says you are a member of. For anyone else only explicit grants on GitHub count: GitHub gives every user read on public repositories, and vx ignores that read, so a public repository does not show up for people outside its organization. Outside collaborators with write, maintain or admin keep it; outside collaborators with only read get nothing in vx.
- **What does not count.** vx grants, teams, the organization's default role and organization roles give nothing on GitHub-backed repositories. `vx repo grant` for people and teams is refused with a link to the repository's access settings on GitHub; `vx repo revoke` still removes grants made before the repository was linked. A vx organization linked to a GitHub organization refuses `vx org member`, `vx org remove` and team changes, with a link to the organization's people page on GitHub.
- **Caching.** vx caches what GitHub said for 5 minutes (`GitHubRoleTTL`) per person and repository, and per person and organization. GitHub webhooks drop the cache at once when access changes: collaborators added, removed or edited; organization membership; teams (the whole organization's cache); repository visibility, rename and transfer; installation removed, suspended or its repositories changed (everything for it); a person removed. Without a webhook, a change shows within 5 minutes. Organization membership is refreshed on sign-in, on organization webhooks and whenever vx asks GitHub again.
- **When GitHub cannot be reached.** vx uses a cached answer up to one hour old. Without one, access fails closed: the command fails with "could not check your role on REPO with GitHub ... try again in a minute", and lists leave the repository out. vx never falls back to its own grants.
- **Installations.** A suspended installation, or one that was removed, gives nobody a role on its repositories (site admins aside) until it is unsuspended or installed again.

**Limits.** A list (repositories, efforts, the inbox) asks GitHub at most once per organization and once per repository of your organizations every 5 minutes, up to 8 calls at a time; for tens of repositories that is well under a second with a cold cache and a few database queries otherwise. Every installation counts against GitHub's API rate limit for the App (5,000 requests per hour per installation and up). Hundreds of repositories per person, or hundreds of installations on one server, would want a batch API (GraphQL) and webhook-driven membership instead of per-repository checks. The access page lists people whose role vx checked in the last 5 minutes, not everyone GitHub allows. The settings links point to github.com.

## Commands

```sh
vx org create acme --name "Acme Inc"            # you become its owner
vx org list                                     # your organizations (site admins: --all)
vx org show acme                                # members, teams, repositories you can read
vx org set-default acme none|read|write         # what every member gets
vx org member acme bob --role owner|admin|member
vx org remove acme bob                          # also removes bob from acme's teams

vx team create acme/backend
vx team add acme/backend bob                    # bob must be a member of acme
vx team remove acme/backend bob
vx team delete acme/backend                     # also deletes its grants

vx repo create acme/api                         # in acme if you own or administer it
vx repo create tools --org acme
vx repo grant acme/api team:acme/backend --role write
vx repo revoke acme/api user:bob
vx repo access acme/api                         # direct grants and everyone's role, with why (repository admins)
```

Every command takes `--json`. Every change is recorded in the operation log (`org.create`, `org.update`, `org.member`, `team.create`, `team.delete`, `team.add`, `team.remove`, `repo.grant`, `repo.revoke`, `person.create`, and for GitHub sign-in `person.signup`, `person.signin`, `person.github_link`, `person.invite`, `person.uninvite`).

## Web UI

The same management is in the web UI (`vx ui` opens it signed in). Pages are server-rendered and every change is a plain form, so no JavaScript is needed. Forms work only from the vx page itself (posts from other sites are refused) and only in a person's session: an agent signed in to the web UI sees what its person can see, but every change is refused with 403. Errors such as demoting the last owner are shown as a message on the page.

| Page | Who sees it | What it shows and does |
|---|---|---|
| **Organizations** (`/ui/orgs`, in the sidebar for everyone) | Every signed-in person | Your organizations with your role, members and repositories. "New organization" creates one with you as owner. Site admins can switch to every organization on the server. |
| **An organization** (`/ui/orgs/ORG`) | Its members, and site admins | Members with their role and teams; teams with their members; the repositories you can read, with the default role and your role; settings (display name, default repository role). Owners, admins and site admins change roles, remove members, add people by handle, create and delete teams, and add or remove team members. Members see the same page read-only. To anyone else the organization does not exist (404). |
| **Repository access** (`/ui/repos/REPO/access`, the Access button on the repository page and the organization page) | Admins of that repository | Direct grants, a form to grant a person or a team read, write or admin (a team named without its organization, like `backend`, is the repository's organization's), and Revoke. Below it, everyone's effective role and where it comes from (`org default`, `team:acme/backend`, `user:alice`, ...). People who can read the repository get 403; everyone else gets 404. |
| **Admin, People** (`/ui/admin/people`, "Admin" in the sidebar) | Site admins | Everyone on the server: their agents, whether they are a site admin, and their organizations with their role. "Add person" creates a person, optionally as a member of an organization and optionally as a site admin. With GitHub sign-in, "Invite by GitHub login" lets a GitHub user create their account by signing in, and the open invites are listed with a Withdraw button. To anyone else the page does not exist (404). |

Adding a person shows their first token on the next page, once: vx stores only a hash, never puts the token in a URL, and cannot show it again. Give it to them privately, not in a shared channel. They run

```sh
curl -fsSL https://vx.example.com/install.sh | sh   # installs vx into ~/.local/bin
vx login --server https://vx.example.com --token TOKEN
vx ui
```

(or `vx login --server https://vx.example.com` without a token: it opens the browser, where they sign in with that token once and approve), and can then create more tokens and their own agents from Settings, and revoke the first token.

## Upgrading from v0.1

v0.1 had no access control: everyone saw everything. The upgrade (migrations 009 and 010) changes nobody's access:

- each distinct owner part of an existing repository name becomes an organization (lowercased: `Acme/api` goes to `acme`); repositories without an owner part go to an organization named `default`,
- every existing person becomes an owner of every one of those organizations,
- every existing person becomes a site admin,
- every person gets a personal organization; an organization from the step above that is named like a person becomes theirs.

Prune afterwards, as a site admin: see [deploy.md](deploy.md#upgrading-to-v02).

## Limits today

- **Sign-in.** With a GitHub App set up, people sign in with GitHub (see [getting-started.md](getting-started.md#signing-in)); new accounts follow the sign-up mode (invite, open or closed). Otherwise tokens: site admins create people with `vx-server user-add`, the web UI (Admin, People) or the API.
- **Agents.** An agent can change any path its person can, and there are no per-agent budgets for lands or check minutes.
- **Scale.** Objects live on the server's disk, so one instance. It scales up well; it does not scale out yet.
- **GitHub.** One token for the whole server, or SSH. No GitHub App yet, so no per-organization installation or fine-grained repository grants on the GitHub side.

## Roadmap

The order below follows what blocks adoption first. Each item is a GitHub issue; contributions are welcome.

### 1. Single sign-on

- OIDC login for the web UI (Okta, Microsoft Entra ID, Google Workspace, GitHub).
- CLI login through the browser: done. `vx login --server URL` shows a code and opens an approval page; see [getting-started.md](getting-started.md#3-install-the-cli-and-log-in).
- Short-lived tokens for agents, issued from the person's session, with expiry and revocation.
- SCIM provisioning so people and teams follow the identity provider.

### 2. Agent governance

- Path scopes per agent (`internal/http/**`), checked on every land.
- Budgets per agent and per person: lands per hour, check minutes, storage.
- Landing policies per path: `fast`, `gated` (checks before landing) and `human` (approval from an owner).
- Trust levels: new agents land gated until they have a track record.

### 3. GitHub App and other hosts

- A vx GitHub App installed per organization: short-lived installation tokens, repository selection, webhooks for upstream pushes and review comments.
- Review feedback mapped back to the change and the agent that wrote the line.
- Adapters for GitLab, Bitbucket, Gitea and Forgejo.

### 4. Scale and operations

- Object storage backends (S3, R2, GCS, MinIO), so `vx-server` is stateless and runs as several replicas.
- A cache tier for objects close to agents and CI.
- Helm chart and Terraform examples.
- Prometheus metrics, structured audit log export (SIEM), retention policies.
- Backup and restore tooling, and documented disaster recovery.

### 5. Collaboration between agents

- Leases on paths and symbols, and subscriptions to changes others make.
- More of the board and history through the MCP endpoint.

## Running vx in an organization today

1. Run vx behind your VPN or SSO proxy (for example an identity-aware proxy in front of the web UI).
2. Create one organization per team or department, add people as members, and set the default role to `none` where repositories should be opt-in.
3. Use a GitHub token limited to the repositories the server connects.
4. Issue tokens per person; have people create their own agents.
5. Back up Postgres and the data volume daily.
6. Keep humans as the only ones who merge pull requests, under your branch protection rules. vx never merges.

## Get involved

Organizations evaluating vx: open a [discussion](https://github.com/TerracodeBA/vx/discussions) describing your setup and needs. It shapes the order of this roadmap.

## Roles and what they allow

A repository role is read, write or admin; each includes the one before it. Agents have exactly the role of the person they act for. A repository you cannot read is reported as not found, in the CLI, the API, the web UI (the normal 404 page) and MCP, so its existence does not leak; one you can read but need more on is a permission error that names the role needed and who can grant it.

| Operation | CLI, MCP | Role |
|---|---|---|
| See a repository, its branches and your role | `vx repo list`, `vx_efforts` | read |
| List and show efforts | `vx effort list`, `vx effort show`, `vx clone`, `vx_status` | read |
| Log, show a change, diff | `vx log`, `vx show`, `vx diff`, `vx_log`, `vx_show`, `vx_diff` | read |
| Show a workspace | `vx status` | read |
| Read the board, context and notes | `vx board`, `vx context`, `vx_board`, `vx_context` | read |
| Inbox and wait (only items in repositories you can read) | `vx inbox`, `vx wait`, `vx_inbox` | read |
| List shipments | `vx ship list` | read |
| List claims and tasks, see a rebase | `vx claims`, `vx tasks`, `vx status`, `vx_claims`, `vx_tasks` | read |
| Import upstream commits | `vx repo sync` | write |
| Start an effort | `vx effort new` | write |
| Claim, add and change tasks (also by id) | `vx claim`, `vx release`, `vx task ...`, `vx_claim`, `vx_task_add`, `vx_task_update` | write |
| Rebase an effort, continue or abort | `vx effort rebase` | write |
| Open, rebase, land or abandon a workspace | `vx new`, `vx sync`, `vx land`, `vx abandon` | write |
| Post notes, ask, answer | `vx post`, `vx ask`, `vx answer`, `vx_post_note`, `vx_ask`, `vx_answer` | write |
| Plan and open a shipment (push `vx/<effort>`, open the PR) | `vx ship plan`, `vx ship open` | admin |
| Grant and revoke access, see who has access (on GitHub-backed repositories only service principals are granted; roles come from GitHub) | `vx repo grant`, `vx repo revoke`, `vx repo access` | admin |

On GitHub-backed repositories the role in the last column is the one GitHub gives you (see [GitHub-backed repositories](#github-backed-repositories)).

Lists (repositories, efforts, the inbox) only contain what you can read. `internal/service/authz_test.go` checks every row of this table for no access, read, write and admin, for a person and their agent, with vx grants (`TestAuthzMatrix`) and with roles from GitHub (`TestAuthzMatrixGitHub`).
