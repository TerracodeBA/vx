# Roadmap

vx is early. The plan below is ordered by what blocks adoption first: teams that do not fully trust each other, then agents that coordinate, then deep git host integration, then large organizations. Each item is a GitHub issue; each release is a [milestone](https://github.com/TerracodeBA/vx/milestones). Comments and pull requests are welcome.

## v0.1: Works end to end (released)

Efforts, workspaces and landing with automatic merges; honest evidence with `vx land --run`; the git bridge for GitHub and any git remote; shipping as clean pull requests; the web UI with live timelines and GitHub-style diffs; the Claude Code plugin; Docker and release binaries.

## v0.2: Safe for teams

Today every signed-in user sees every repository. This release makes one server safe for a team that does not fully trust each other.

- [#1](https://github.com/TerracodeBA/vx/issues/1) Organizations, teams and repository roles
- [#2](https://github.com/TerracodeBA/vx/issues/2) Serve objects only through repositories the caller can read
- [#3](https://github.com/TerracodeBA/vx/issues/3) Admin pages in the web UI: people, agents, tokens, repositories
- [#13](https://github.com/TerracodeBA/vx/issues/13) Syntax highlighting in web UI diffs

## v0.3: Agents that coordinate

What the first multi-agent runs showed agents need most.

- [#5](https://github.com/TerracodeBA/vx/issues/5) Path scopes and budgets for agents
- [#6](https://github.com/TerracodeBA/vx/issues/6) Landing policies per path: fast, gated, human
- [#11](https://github.com/TerracodeBA/vx/issues/11) Effort board: typed notes between agents
- [#12](https://github.com/TerracodeBA/vx/issues/12) Leases, subscriptions and an MCP endpoint for agents
- [#14](https://github.com/TerracodeBA/vx/issues/14) Warn about semantic conflicts between concurrent changes

## v0.4: Deep GitHub integration

- [#7](https://github.com/TerracodeBA/vx/issues/7) GitHub App with webhooks and review feedback
- [#15](https://github.com/TerracodeBA/vx/issues/15) Rebase open efforts automatically when the target branch moves
- [#16](https://github.com/TerracodeBA/vx/issues/16) Prepare PR screen: split an effort into independent or stacked pull requests
- [#17](https://github.com/TerracodeBA/vx/issues/17) Code-aware merging (tree-sitter) for fewer false conflicts
- [#8](https://github.com/TerracodeBA/vx/issues/8) GitLab, Bitbucket, Gitea and Forgejo adapters

## v0.5: Ready for organizations

- [#4](https://github.com/TerracodeBA/vx/issues/4) Single sign-on: OIDC for the web UI and browser login for the CLI
- [#9](https://github.com/TerracodeBA/vx/issues/9) Object storage backends for stateless replicas
- [#10](https://github.com/TerracodeBA/vx/issues/10) Prometheus metrics, audit log export, Helm chart
- [#18](https://github.com/TerracodeBA/vx/issues/18) Partial and on-demand checkouts for large repositories

## vx Cloud

A hosted vx, for teams that do not want to run their own server, is planned alongside v0.2 and v0.3. Self-hosting stays free and fully featured under Apache-2.0.

See [docs/organizations.md](docs/organizations.md) for the reasoning behind this order.
