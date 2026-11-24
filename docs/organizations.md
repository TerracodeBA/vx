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
- **Superadmins** run the server: people, invites and sign-up, the GitHub App, server settings and the operation log's metadata. They see every organization and person, but have **no role on any repository** and change no organization they do not own or administer. See [Superadmins and support access](#superadmins-and-support-access). `vx-server user-add --admin` makes one.

## Repository roles

A repository role is `read`, `write` or `admin`; each includes the one before it. Your role on a repository is the highest of:

1. a direct grant to you (`vx repo grant acme/api alice --role write`),
2. a grant to a team you are in (`vx repo grant acme/api team:acme/backend --role write`),
3. the organization's default role, if you are a member,
4. admin, if you are an owner or admin of the organization,
5. read or write, while a support grant to you lasts (superadmins only, see below).

Being a superadmin gives no role. On a GitHub-backed repository none of these but support grants count: your role is your GitHub permission (see below).

**Agents** have exactly their person's role. **Service principals** (`service:ci`) have only their direct grants. Grants go to people (`alice` or `user:alice`), service principals (`service:ci`) and teams of the repository's own organization (`team:acme/backend`), never to agents.

A repository you cannot read is reported exactly like one that does not exist. One you can read but need more on gives a permission error that names the role needed and who can grant it.

Only people manage organizations, teams and grants. Agents are refused, like token creation.

## Superadmins and support access

A superadmin is a person who runs the vx server. On vx Cloud that is the operator; on a self-hosted server, whoever administers it. The role is limited to running the server:

- **The admin panel:** people (Admin, People: add people, see everyone with their organizations and agents), invites and the sign-up mode, the GitHub App (Admin, GitHub), server settings, agents' scopes and budgets (to stop a runaway agent; a scope only narrows a role), and the operation log's metadata (who did what when, on which repository, without the content).
- **No implicit role on any repository.** To a superadmin without a role, a customer's repository is "not found", exactly like to anyone else: in the CLI, the API, the web UI and MCP, for files and objects, changes, diffs, efforts, board notes, claims, tasks, overlaps, reviews, the inbox and repository lists. Their agents have the same nothing.
- **Organizations are read-only to them.** A superadmin sees every organization, its members and teams (people administration), and only the repositories they can read. They cannot change an organization's members, teams or settings, or create repositories in it, unless they are its owner or admin: adding themselves would give them its repositories.
- **Ordinary roles still count.** A superadmin who is a member of an organization, or whom a repository's admins grant a role, has that role like anyone else. On a self-hosted server the admin usually owns the team's organization; that is how they keep access.

**Support access.** When a customer wants the operator to look at a repository (a failing land, a stuck shipment), the repository's admins give a superadmin support access: read (the default) or write on that one repository, for a limited time.

```sh
vx repo support acme/api root --role read --for 72h --reason "ticket 4711: land fails"
vx repo support acme/api                          # what is in effect, until when, granted by whom
vx repo support acme/api root --revoke            # end it now
```

- It lasts 24 hours unless `--for` says otherwise, at most 720 hours (30 days), and ends by itself. Granting again replaces the earlier grant.
- It is never admin, so it cannot be used to change who has access or to ship. A customer who wants more grants an ordinary role (`vx repo grant`) and revokes it afterwards.
- Only superadmins can receive it, and it counts only while they are one. It works on GitHub-backed repositories too, where vx grants otherwise count for nothing.
- It shows on the access page and in `vx repo access` as "operator (support access) until DATE", in the **Support access** panel of the access page with the reason and who granted it, and in the operation log (`repo.support.grant`, `repo.support.revoke`, with the person, role, expiry and reason).
- The repository's admins revoke it at any time (End now on the access page). The superadmin may end their own early.

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

- **Who has a role.** People linked to GitHub (they signed in with GitHub). A person without a GitHub link has no role on GitHub-backed repositories. Superadmins have none from being one; support access counts. Agents have exactly their person's role. Service principals (`service:ci`) have only their direct vx grants, since GitHub does not know them; repository admins may grant them.
- **Your account or organization.** You get a role on a repository of an installation on your own GitHub account, or of an organization GitHub says you are a member of. For anyone else only explicit grants on GitHub count: GitHub gives every user read on public repositories, and vx ignores that read, so a public repository does not show up for people outside its organization. Outside collaborators with write, maintain or admin keep it; outside collaborators with only read get nothing in vx.
- **What does not count.** vx grants, teams, the organization's default role and organization roles give nothing on GitHub-backed repositories. `vx repo grant` for people and teams is refused with a link to the repository's access settings on GitHub; `vx repo revoke` still removes grants made before the repository was linked. A vx organization linked to a GitHub organization refuses `vx org member`, `vx org remove` and team changes, with a link to the organization's people page on GitHub.
- **Caching.** vx caches what GitHub said for 5 minutes (`GitHubRoleTTL`) per person and repository, and per person and organization. GitHub webhooks drop the cache at once when access changes: collaborators added, removed or edited; organization membership; teams (the whole organization's cache); repository visibility, rename and transfer; installation removed, suspended or its repositories changed (everything for it); a person removed. Without a webhook, a change shows within 5 minutes. Organization membership is refreshed on sign-in, on organization webhooks and whenever vx asks GitHub again.
- **When GitHub cannot be reached.** vx uses a cached answer up to one hour old. Without one, access fails closed: the command fails with "could not check your role on REPO with GitHub ... try again in a minute", and lists leave the repository out. vx never falls back to its own grants.
- **Installations.** A suspended installation, or one that was removed, gives nobody a role on its repositories (service principals and support access aside) until it is unsuspended or installed again. A repository removed from an installation, or deleted on GitHub, stays GitHub-backed with nobody but service principals and support access having a role: grants from before it was linked never come back.

**Limits.** A list (repositories, efforts, the inbox) asks GitHub at most once per organization and once per repository of your organizations every 5 minutes, up to 8 calls at a time; for tens of repositories that is well under a second with a cold cache and a few database queries otherwise. Every installation counts against GitHub's API rate limit for the App (5,000 requests per hour per installation and up). Hundreds of repositories per person, or hundreds of installations on one server, would want a batch API (GraphQL) and webhook-driven membership instead of per-repository checks. The access page lists people whose role vx checked in the last 5 minutes, not everyone GitHub allows. The settings links point to github.com.

## Commands

```sh
vx org create acme --name "Acme Inc"            # you become its owner
vx org list                                     # your organizations (superadmins: --all)
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
vx repo support acme/api root --for 24h         # support access for a superadmin (repository admins)
```

Every command takes `--json`. Every change is recorded in the operation log (`org.create`, `org.update`, `org.member`, `team.create`, `team.delete`, `team.add`, `team.remove`, `repo.grant`, `repo.revoke`, `repo.support.grant`, `repo.support.revoke`, `person.create`, `server.notice.resolve`, and for GitHub sign-in `person.signup`, `person.signin`, `person.github_link`, `person.invite`, `person.uninvite`).

## Web UI

The same management is in the web UI (`vx ui` opens it signed in). Pages are server-rendered and every change is a plain form, so no JavaScript is needed. Forms work only from the vx page itself (posts from other sites are refused) and only in a person's session: an agent signed in to the web UI sees what its person can see, but every change is refused with 403. Errors such as demoting the last owner are shown as a message on the page.

| Page | Who sees it | What it shows and does |
|---|---|---|
| **Reviews** (`/ui/reviews`, in the sidebar for everyone) | Every signed-in person, for the repositories they can read | Changes waiting for a person under a human landing policy, with who landed them and the files under the policy, and the recently decided ones. Each links to its change page, where people who may decide it Approve (it lands) or Reject with a reason. |
| **Organizations** (`/ui/orgs`, in the sidebar for everyone) | Every signed-in person | Your organizations with your role, members and repositories. "New organization" creates one with you as owner. Superadmins can switch to every organization on the server. |
| **An organization** (`/ui/orgs/ORG`) | Its members, and superadmins (read-only, without the repositories they cannot read) | Members with their role and teams; teams with their members; the repositories you can read, with the default role and your role; settings (display name, default repository role). Owners and admins change roles, remove members, add people by handle, create and delete teams, and add or remove team members. Members see the same page read-only. To anyone else the organization does not exist (404). |
| **Repository access** (`/ui/repos/REPO/access`, the Access button on the repository page and the organization page) | Admins of that repository | Direct grants, a form to grant a person or a team read, write or admin (a team named without its organization, like `backend`, is the repository's organization's), and Revoke. Support access: the superadmins who have it, their role, until when, the reason and who granted it, with End now, and a form to give a superadmin read or write for 4 hours to 30 days. Below it, everyone's effective role and where it comes from (`org default`, `team:acme/backend`, `user:alice`, `operator (support access) until DATE`, ...). People who can read the repository get 403; everyone else gets 404. |
| **Admin, People** (`/ui/admin/people`, "Admin" in the sidebar) | Superadmins | Everyone on the server: their agents, whether they are a superadmin, and their organizations with their role. "Add person" creates a person, optionally as a member of an organization and optionally as a superadmin. After an upgrade from a version with site admins, a review banner says that site admins no longer have access to repositories, until a superadmin clicks Mark as reviewed (see [deploy.md](deploy.md#upgrading-to-v05-superadmins)). With GitHub sign-in, "Invite by GitHub login" lets a GitHub user create their account by signing in, and the open invites are listed with a Withdraw button. To anyone else the page does not exist (404). |
| **Admin, Audit** (`/ui/admin/audit`, linked from People and from organization pages) | Superadmins (the whole server, metadata of repository entries) and owners and admins of an organization (their organization) | The newest 200 entries of the operation log for the filters (organization, repository, actor, time), and downloads of every match as JSONL or CSV. Superadmins also see the data disk's free space here, and a warning on every page when it runs low. See [Audit log](#audit-log). |
| **Admin, GitHub** (`/ui/admin/github`, linked from People) | Superadmins | The server's GitHub App: name, App and client id, and the link to install it on GitHub organizations and accounts. "Create the GitHub App" posts vx's App manifest to GitHub (under your account or an organization you own), and "Finish setting up GitHub" on the page GitHub sends you back to stores its credentials encrypted with `VX_SECRET_KEY`. Agents see the page but cannot create the App. To anyone else the page does not exist (404). |

Adding a person shows their first token on the next page, once: vx stores only a hash, never puts the token in a URL, and cannot show it again. Give it to them privately, not in a shared channel. They run

```sh
curl -fsSL https://vx.example.com/install.sh | sh   # installs vx into ~/.local/bin
vx login --server https://vx.example.com --token TOKEN
vx ui
```

(or `vx login --server https://vx.example.com` without a token: it opens the browser, where they sign in with that token once and approve), and can then create more tokens and their own agents from Settings, and revoke the first token.

## Landing policies: who approves

A `[[policy]]` with `mode = "human"` in a repository's `vx.toml` keeps changes to its paths out of the effort until a person approves them (see [docs/agents.md](agents.md#landing-policies)). Who may approve or reject such a review:

- **A person.** Agents never decide reviews, not even for their own person: `vx review approve` with an agent token and the web UI signed in as an agent are refused (`only people approve or reject reviews`).
- **With write on the repository.** On a GitHub-backed repository that is write (or more) on GitHub; vx grants do not count there. Someone who cannot read the repository does not see the review at all.
- **In every `approvers` list** of the matching human policies, when they name approvers: `user:HANDLE` is the person with that vx handle (with GitHub sign-in, the GitHub login), `team:ORG/TEAM` a member of that vx team. GitHub-linked organizations have no vx teams, so on them use `user:` entries.
- **Not the change's author.** A person who landed the change by hand needs another person. A person may approve the changes of their own agents: the policy asks for a person to look, and agents act for their person. If you need four eyes on agent work too, name approvers who are not the agents' owners.

Approving lands the change onto the effort head as it is then, authored by the agent, with the approver in its evidence. Rejecting needs a reason, which the author reads in a note addressed to them. The web UI's **Reviews** page lists what waits, marks what you can decide, and links each review to its change page with Approve and Reject (form posts, same-origin only).

## Upgrading from v0.1

v0.1 had no access control: everyone saw everything. The upgrade (migrations 009 and 010) changes nobody's access:

- each distinct owner part of an existing repository name becomes an organization (lowercased: `Acme/api` goes to `acme`); repositories without an owner part go to an organization named `default`,
- every existing person becomes an owner of every one of those organizations,
- every existing person becomes a site admin (since v0.5 a superadmin, without access to repositories: see [deploy.md](deploy.md#upgrading-to-v05-superadmins)),
- every person gets a personal organization; an organization from the step above that is named like a person becomes theirs.

Prune afterwards, as a superadmin: see [deploy.md](deploy.md#upgrading-to-v02).

## Quotas

On a shared server one organization must not be able to exhaust it. Each organization has quotas; the server's configuration sets the defaults for all (`VX_QUOTA_*`, see [deploy.md](deploy.md#rate-limits-and-quotas)) and superadmins override them for one organization.

| Quota | Counts | Refused |
|---|---|---|
| Repositories | the organization's repositories | `vx repo create` |
| Stored data | bytes the organization's objects take in the object store, each object once | uploads (`vx land`, `vx sync`) and imports that would go over it |
| One import | the size of a repository on its git host, asked before anything is downloaded (GitHub, GitLab, Gitea, Forgejo and Bitbucket report it; plain git remotes cannot be asked) | `vx repo create` with a remote |
| One upload | the bytes of one upload of objects (the CLI sends a land's new files in uploads of up to 64 MiB) | `vx land`, `vx sync` |
| Open efforts | efforts that are open or shipping | `vx effort new` |

A refusal says what is used, what the limit is and what to do, for example `organization acme has 10 of its 10 repositories (quota): remove one first, or ask a superadmin to raise the quota (vx-server quota acme, or Admin > Organizations > acme)`. The API answers `ResourceExhausted`.

Members see the quotas and what is used on the organization's page (Quotas). Superadmins change them there, or on the server:

```sh
vx-server quota acme                                  # what acme uses, and its limits
vx-server quota acme --repos 50 --stored 20GB         # override two defaults
vx-server quota acme --push default --open-efforts 0  # back to the default; 0 means no limit
```

Changes are in the audit log as `org.quota`. Stored data shows "not measured yet" until the server counts object sizes (object storage, v0.5).

## Audit log

Every change in vx writes an entry to the operation log: who (person or agent), what (`effort.create`, `ship`, `org.member`, `token.revoke`, ...), when, in which organization and repository, and on what. Export it for your SIEM or for a review:

```sh
vx admin audit --org acme --since 30d > acme.jsonl                         # one JSON object per line
vx admin audit --org acme --actor agent:alice/claude --format csv -o a.csv
vx admin audit --since 2026-11-01 --until 2026-11-30                     # superadmins: the whole server
```

Filters: `--org`, `--repo`, `--actor`, `--since`, `--until` (dates, RFC 3339 times or ages like `7d`). The web UI has the same as **Admin > Audit** (`/ui/admin/audit`), with the newest 200 entries and JSONL and CSV downloads; the organization page links to it for its owners and admins. Columns: `id, at, op, org, repo, actor, target, before, after`.

Who sees what:

- **Owners and admins of an organization** export that organization: its organization-level entries (members, teams, quotas) and the entries of its repositories, with `before` and `after` (what changed) for the repositories they administer.
- **Superadmins** export the whole server, but only the metadata of repository entries: `before` and `after`, which can hold intents, titles and paths, stay in the database. They see the full server-level entries (people, invites, tokens).
- Members, and anyone else, cannot export.

Entries are kept forever unless the server sets a retention (`VX_AUDIT_RETENTION`, for example `365d`; at least 7 days); older entries are deleted once an hour.

## Limits today

- **Sign-in.** With a GitHub App set up, people sign in with GitHub (see [getting-started.md](getting-started.md#signing-in)); new accounts follow the sign-up mode (invite, open or closed). Otherwise tokens: superadmins create people with `vx-server user-add`, the web UI (Admin, People) or the API.
- **Agents.** A person can limit each agent to some paths and give it budgets for lands per day, lines and files per land and open workspaces (see [agents.md](agents.md#scopes-and-budgets)). There are no budgets per person, for check minutes or for storage yet.
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

- Path scopes per agent (`internal/http/**`), checked on every land: done, `vx agent scope` (see [agents.md](agents.md#scopes-and-budgets)).
- Budgets per agent: lands per day, lines and files per land, open workspaces: done. Still to come: budgets per person, check minutes, storage.
- Landing policies per path: `fast`, `gated` (checks before landing) and `human` (a person approves): done, `[[policy]]` in vx.toml and `vx review` (see [Landing policies: who approves](#landing-policies-who-approves)).
- Trust levels: new agents land gated until they have a track record.

### 3. GitHub App and other hosts

- A vx GitHub App installed per organization: short-lived installation tokens, repository selection, webhooks for upstream pushes and review comments.
- Review feedback mapped back to the change and the agent that wrote the line.
- Adapters for GitLab, Bitbucket, Gitea and Forgejo (done in v0.4, with vx's own roles); roles read from those hosts' permissions.

### 4. Scale and operations

- Object storage backends (S3, R2, GCS, MinIO), so `vx-server` is stateless and runs as several replicas.
- A cache tier for objects close to agents and CI.
- Helm chart: done in v0.5 (`deploy/helm/vx`, see [deploy.md](deploy.md#kubernetes-helm)). Terraform examples: later.
- Prometheus metrics, audit log export (JSONL, CSV) and retention: done in v0.5 (see [Audit log](#audit-log) and [deploy.md](deploy.md#metrics)).
- Rate limits and quotas per organization: done in v0.5 (see [Quotas](#quotas)).
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

A repository role is read, write or admin; each includes the one before it. Agents have exactly the role of the person they act for; a [scope](agents.md#scopes-and-budgets) can narrow what they may change within it, never widen it. A repository you cannot read is reported as not found, in the CLI, the API, the web UI (the normal 404 page) and MCP, so its existence does not leak; one you can read but need more on is a permission error that names the role needed and who can grant it.

| Operation | CLI, MCP | Role |
|---|---|---|
| See a repository, its branches and your role | `vx repo list`, `vx_efforts` | read |
| List and show efforts | `vx effort list`, `vx effort show`, `vx clone`, `vx_status` | read |
| Log, show a change, diff | `vx log`, `vx show`, `vx diff`, `vx_log`, `vx_show`, `vx_diff` | read |
| Show a workspace | `vx status` | read |
| Read the board, context and notes | `vx board`, `vx context`, `vx_board`, `vx_context` | read |
| Inbox and wait (only items in repositories you can read) | `vx inbox`, `vx wait`, `vx_inbox` | read |
| For you page and push notifications (only items in repositories you can read; turning notifications on is per person and browser, people only) | web For you page, Settings > Notifications | read |
| Mark notes read (notes in repositories you cannot read are skipped) | `vx board`, `vx context`, `vx inbox`, `vx wait`, the sync feed, MCP reads | read |
| List shipments | `vx ship list` | read |
| CI results of shipments | `vx ship list`, `vx status`, web ship page | read |
| See the vx.toml rules and reservations | `vx rules`, `vx_rules` | read |
| List claims and tasks, see a rebase | `vx claims`, `vx tasks`, `vx status`, `vx_claims`, `vx_tasks` | read |
| See overlap forecasts with other efforts (a stale one is recomputed first) | `vx overlaps`, `vx status`, `vx land`, `vx_overlaps`, the web effort page and badges | read |
| Import upstream commits | `vx repo sync` | write |
| Start an effort | `vx effort new` | write |
| Claim, add and change tasks (also by id) | `vx claim`, `vx release`, `vx task ...`, `vx_claim`, `vx_task_add`, `vx_task_update` | write |
| Rebase an effort, continue or abort | `vx effort rebase` | write |
| Close an effort (merged or abandoned) | `vx effort close` | write |
| Reserve the next key of a vx.toml `[[unique]]` rule for an effort | `vx reserve`, `vx_reserve` | write |
| Open, rebase, land or abandon a workspace | `vx new`, `vx sync`, `vx land`, `vx abandon` | write |
| List and show changes waiting for a person (landing policies) | `vx review list`, `vx review show`, `vx status`, Reviews and the effort page | read |
| Approve or reject a pending review (a person, not the change's author, in the policy's approvers when it names them; never an agent) | `vx review approve`, `vx review reject`, Approve and Reject on the change page | write |
| Report a workspace's changed files (your own workspace) | `vx status`, `vx sync` | write |
| Post notes, ask, answer | `vx post`, `vx ask`, `vx answer`, `vx_post_note`, `vx_ask`, `vx_answer` | write |
| Plan and open a shipment (push `vx/<effort>`, open the PR) | `vx ship plan`, `vx ship open` | admin |
| Set an agent's path scope and budgets (no repository role: only the agent's person or a superadmin; never an agent, not even for itself) | `vx agent scope`, Settings | person or superadmin |
| See an agent's scope, budgets and usage (the agent itself, its person, superadmins) | `vx agent show`, `vx whoami`, `vx status`, `vx_whoami`, Settings | person, agent or superadmin |
| Plan and open a split shipment (one `vx/<effort>/<n>-<name>` branch and PR per part) | `vx ship plan --split`, `vx ship open --split`, the Prepare pull requests page | admin |
| Set a GitLab, Gitea, Forgejo or Bitbucket repository's token, make a new webhook secret | `vx repo host` | admin |
| Grant and revoke access, see who has access (on GitHub-backed repositories only service principals are granted; roles come from GitHub) | `vx repo grant`, `vx repo revoke`, `vx repo access` | admin |
| Give a superadmin support access, list it, end it (read or write, at most 30 days; also on GitHub-backed repositories) | `vx repo support`, Support access on the access page | admin (the superadmin may end their own) |
| Run the server: people, invites and sign-up, the GitHub App, server settings, operation log metadata, the upgrade review | `vx-server`, Admin pages, `CreatePerson`, `ListPeople`, `vx org list --all` | superadmin (no repository role) |

A superadmin has none of the repository roles in this table from being one: every repository operation reports "not found" to them until they have a role or support access (tested in `TestAuthzMatrix` and `TestAuthzMatrixGitHub`).
| See an organization's quotas and usage (no repository role: members of the organization and superadmins) | the organization page, `vx-server quota ORG` | member or superadmin |
| Change an organization's quotas (never an agent) | the organization page, `vx-server quota ORG --repos N ...` | superadmin |
| Export an organization's audit log (with what changed for repositories you administer) | `vx admin audit --org ORG`, Admin > Audit | organization owner or admin |
| Export the whole server's audit log (metadata only for repository entries) | `vx admin audit`, Admin > Audit | superadmin |

On GitHub-backed repositories the role in the last column is the one GitHub gives you (see [GitHub-backed repositories](#github-backed-repositories)).

An agent's [scope and budgets](agents.md#scopes-and-budgets) come on top of its role: writes need the role first, then the scope. A refused path or a spent budget is a permission or quota error that names who can widen it; it never hides a repository the agent can read.

GitLab, Gitea, Forgejo and Bitbucket repositories use vx's roles in this table (grants, teams, organization defaults), never the host's permissions: someone with Maintainer on GitLab has no role in vx until granted one, and a vx admin may ship there even without a role on the host (vx pushes and opens the merge request with the repository's token).

Lists (repositories, efforts, the inbox) only contain what you can read. `internal/service/authz_test.go` checks every row of this table for no access, read, write and admin, for a person and their agent, with vx grants (`TestAuthzMatrix`) and with roles from GitHub (`TestAuthzMatrixGitHub`).
