# vx for organizations

vx works well today for individuals and small teams who trust each other. Organizations with many people, many teams and compliance needs require more. This page is honest about where vx stands and lays out the plan.

## Three ways to run vx

| | Individual | Team | Organization |
|---|---|---|---|
| Who | One person and their agents | A small group that trusts each other | Many teams, roles, audits |
| Where | Laptop or workstation | One shared server | Several replicas behind a load balancer |
| Sign-in | Token | Token per person | Single sign-on (OIDC, SAML) |
| Access | Everything | Everyone sees every repo | Per org, team and repo roles |
| Agents | Your own | Each person's own | Scoped to paths, with budgets |
| GitHub | SSH or personal token | Shared token | GitHub App per organization |
| Storage | Local disk | Server disk | Object storage (S3, R2, GCS) |
| Status | Ready | Ready, with the limits below | Planned |

## Limits today

- **Visibility.** Every signed-in user can see and land into every repository on the server. Run one server per trust boundary.
- **Objects.** File contents are addressed by hash and shared across repositories on a server. Anyone who can sign in and knows a hash can fetch that object.
- **Sign-in.** Tokens only. Admins create people with `vx-server user-add`; there is no admin UI yet.
- **Agents.** An agent can change any path, and there are no per-agent budgets for lands or check minutes.
- **Scale.** Objects live on the server's disk, so one instance. It scales up well; it does not scale out yet.
- **GitHub.** One token for the whole server, or SSH. No GitHub App yet, so no per-organization installation or fine-grained repository grants.

## Roadmap to organizations

The order below follows what blocks adoption first. Each item is a GitHub issue; contributions are welcome.

### 1. Access control

- Organizations and teams as first-class objects; repositories belong to an organization.
- Roles per repository: admin, maintainer, member, viewer. Agents inherit at most their person's role.
- Objects served only through repositories the caller can read.
- Admin pages in the web UI: people, agents, tokens (create, revoke), repositories.

### 2. Single sign-on

- OIDC login for the web UI (Okta, Microsoft Entra ID, Google Workspace, GitHub).
- CLI login through the browser (`vx login` opens a device flow) instead of pasted tokens.
- Short-lived tokens for agents, issued from the person's session, with expiry and revocation.
- SCIM provisioning so people and teams follow the identity provider.

### 3. Agent governance

- Path scopes per agent (`internal/http/**`), checked on every land.
- Budgets per agent and per person: lands per hour, check minutes, storage.
- Landing policies per path: `fast`, `gated` (checks before landing) and `human` (approval from an owner).
- Trust levels: new agents land gated until they have a track record.

### 4. GitHub App and other hosts

- A vx GitHub App installed per organization: short-lived installation tokens, repository selection, webhooks for upstream pushes and review comments.
- Review feedback mapped back to the change and the agent that wrote the line.
- Adapters for GitLab, Bitbucket, Gitea and Forgejo.

### 5. Scale and operations

- Object storage backends (S3, R2, GCS, MinIO), so `vx-server` is stateless and runs as several replicas.
- A cache tier for objects close to agents and CI.
- Helm chart and Terraform examples.
- Prometheus metrics, structured audit log export (SIEM), retention policies.
- Backup and restore tooling, and documented disaster recovery.

### 6. Collaboration between agents

- The effort board: typed notes between agents (plan, decision, interface, question, answer, blocker, handoff), attached to files and symbols, delivered as information and never as instructions.
- Leases on paths and symbols, and subscriptions to changes others make.
- An MCP endpoint so agents query vx directly ("who calls this", "who is working on this").

## Running vx in an organization today

If you want to start now:

1. Run one vx server per team or trust boundary, behind your VPN or SSO proxy (for example an identity-aware proxy in front of the web UI).
2. Use a GitHub token limited to that team's repositories.
3. Issue tokens per person; have people create their own agents.
4. Back up Postgres and the data volume daily.
5. Keep humans as the only ones who merge pull requests, under your branch protection rules. vx never merges.

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
| Import upstream commits | `vx repo sync` | write |
| Start an effort | `vx effort new` | write |
| Open, rebase, land or abandon a workspace | `vx new`, `vx sync`, `vx land`, `vx abandon` | write |
| Post notes, ask, answer | `vx post`, `vx ask`, `vx answer`, `vx_post_note`, `vx_ask`, `vx_answer` | write |
| Plan and open a shipment (push `vx/<effort>`, open the PR) | `vx ship plan`, `vx ship open` | admin |
| Grant and revoke access, see who has access | `vx repo grant`, `vx repo revoke`, `vx repo access` | admin |

Lists (repositories, efforts, the inbox) only contain what you can read. `internal/service/authz_test.go` checks every row of this table for no access, read, write and admin, for a person and their agent.
