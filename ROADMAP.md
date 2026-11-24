# Roadmap

vx is early. The plan below is ordered by what blocks adoption first: teams that do not fully trust each other, then agents that coordinate, then deep git host integration, then large organizations. Each item is a GitHub issue; each release is a [milestone](https://github.com/TerracodeBA/vx/milestones). Comments and pull requests are welcome.

## v0.1: Works end to end (released)

Efforts, workspaces and landing with automatic merges; honest evidence with `vx land --run`; the git bridge for GitHub and any git remote; shipping as clean pull requests; the web UI with live timelines and GitHub-style diffs; the Claude Code plugin; Docker and release binaries.

## v0.2: Safe for teams (done)

Before v0.2 every signed-in user saw every repository. v0.2 makes one server safe for a team that does not fully trust each other.

- [#1](https://github.com/TerracodeBA/vx/issues/1) Organizations, teams and repository roles
- [#2](https://github.com/TerracodeBA/vx/issues/2) Serve objects only through repositories the caller can read
- [#3](https://github.com/TerracodeBA/vx/issues/3) Admin pages in the web UI: people, agents, tokens, repositories
- [#13](https://github.com/TerracodeBA/vx/issues/13) Syntax highlighting in web UI diffs

## v0.3: Agents that coordinate

What the first multi-agent runs showed agents need most. Next up: efforts that see each other, because two efforts changing the same code today only find out when the second one ships.

- [#26](https://github.com/TerracodeBA/vx/issues/26) Efforts see each other: repo-wide context, overlap forecast, repo rules (`vx.toml`), reservations
- [#27](https://github.com/TerracodeBA/vx/issues/27) Board polish: what agents reported while coordinating
- [#5](https://github.com/TerracodeBA/vx/issues/5) Path scopes and budgets for agents
- [#6](https://github.com/TerracodeBA/vx/issues/6) Landing policies per path: fast, gated, human (done: `[[policy]]` in vx.toml, `vx review`, Reviews in the web UI)
- [#11](https://github.com/TerracodeBA/vx/issues/11) Effort board: typed notes between agents (done)
- [#12](https://github.com/TerracodeBA/vx/issues/12) Leases, subscriptions and an MCP endpoint for agents (done: claims, tasks, the note feed, MCP)
- [#14](https://github.com/TerracodeBA/vx/issues/14) Warn about semantic conflicts between concurrent changes

## v0.4: Deep GitHub integration

- [#7](https://github.com/TerracodeBA/vx/issues/7) GitHub App with webhooks and review feedback
- [#15](https://github.com/TerracodeBA/vx/issues/15) Rebase open efforts automatically when the target branch moves (manual `vx effort rebase` done; automatic with #7)
- [#16](https://github.com/TerracodeBA/vx/issues/16) Prepare PR screen: split an effort into independent or stacked pull requests
- [#17](https://github.com/TerracodeBA/vx/issues/17) Code-aware merging (tree-sitter) for fewer false conflicts
- [#8](https://github.com/TerracodeBA/vx/issues/8) GitLab, Bitbucket, Gitea and Forgejo adapters (pull requests, webhooks for pushes, merges, reviews and CI, vx's own roles; host-backed roles later)

## v0.5: Ready for organizations

- [#4](https://github.com/TerracodeBA/vx/issues/4) Single sign-on: OIDC for the web UI and browser login for the CLI
- [#9](https://github.com/TerracodeBA/vx/issues/9) Object storage backends for stateless replicas
- [#10](https://github.com/TerracodeBA/vx/issues/10) Prometheus metrics, audit log export, Helm chart
- [#18](https://github.com/TerracodeBA/vx/issues/18) Partial and on-demand checkouts for large repositories

## vx Cloud

A hosted vx, for teams that do not want to run their own server, is planned alongside v0.2 and v0.3. Self-hosting stays free and fully featured under Apache-2.0.

Before vx Cloud holds code from more than one customer:

- [#33](https://github.com/TerracodeBA/vx/issues/33) Superadmin role for the admin panel, without access to repository content
- [#40](https://github.com/TerracodeBA/vx/issues/40) Delete an organization and all its data
- [#41](https://github.com/TerracodeBA/vx/issues/41) Accept the data processing agreement in the app

See [docs/organizations.md](docs/organizations.md) for the reasoning behind this order.
