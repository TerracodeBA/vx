# Working with agents

vx is built for many agents working at once. This page describes the workflow they follow and what vx records about their work. The same content, written for Claude, is in the plugin skill [`plugins/vx/skills/work/SKILL.md`](../plugins/vx/skills/work/SKILL.md).

## Identities

People create agents that act for them:

```sh
vx agent add backend --env-file ~/.vx-agents/backend.env
```

The agent's identity is `agent:alice/backend`, acting for `user:alice`. Every change it lands records both, plus the model (`VX_MODEL`) and session (`VX_SESSION`) from its environment. Shipped commits are authored by the person, with the agent in a `Vx-Agent` trailer.

Tokens are managed by their people, without an admin, from the CLI or the Settings page of the web UI:

```sh
vx token list --agent backend                                  # when each token was last used
vx token create --agent backend --env-file ~/.vx-agents/backend-2.env
vx token revoke 1a2b3c4d                                       # stops working at once
```

Only people create tokens, for themselves or their own agents. An agent can list and revoke its own tokens but not create one, so a leaked agent token cannot mint replacements that survive its revocation.

## The loop

```sh
vx sync                                   # pick up others' work
vx status                                 # your changes, effort drift, who else is working, recent lands
vx new "Add rate limit middleware"        # one task, one intent
# edit
vx land --run "go test -count=1 ./..." --run "go vet ./..."
```

For Go, pass `-count=1` so every test runs: otherwise `go test` reuses cached results, and vx notes on the check how many packages came from the cache (for example `12 of 14 packages from the Go test cache`).

- One intent per workspace. The intent becomes the change description and, when shipped, the commit message.
- `vx land` closes the workspace; run `vx new` for the next task.
- Agents should never use git inside a vx working copy.

## Evidence

| Flag | Recorded as | Meaning |
|---|---|---|
| `--run "CMD"` | run by vx | vx ran the command in the working copy and recorded the result and duration. If the effort moved, vx synced first and ran it on the merged result, so it describes exactly what lands. |
| `--check "NAME=pass:N"` | reported | The agent says it ran something. Shown, but marked as reported. |
| `--new-test NAME` | new test | A test the change adds or changes (repeatable, or comma-separated). vx refuses the land unless each name is a `func NAME(` in a `_test.go` file the land adds or changes, and lists the test functions it did find; `--allow-unverified-tests` skips the check. |
| `--depends-on SYMBOL` | depends on | A symbol the change relies on. |

When an agent lands without `--run` and others landed first, the server merges the change onto their work. vx then marks the change "merged by server" and labels its evidence as gathered before the merge, in the CLI, the web UI, the commit trailers and the pull request.

`--require-up-to-date` refuses to land if anyone landed since the last sync.

## Conflicts

- Exit code 2 from `vx land` or `vx sync` means conflicts.
- `vx sync` writes diff3-style markers into files both sides changed on the same lines; other conflicts keep one side and say so.
- `vx sync` also lists the files both sides changed that merged cleanly ("Merged with your edits", `merged` in `--json`): they now mix your lines and theirs, so look at them before landing.
- `vx land` refuses files that still contain markers.
- Semantic conflicts (both sides add the same function in different files) merge as text but break the build. `--run` catches them.

## Waiting for each other

```sh
vx wait --timeout 10m && vx sync
```

`vx wait` blocks until someone lands in the effort and lists what landed.

## Running several agents

Plan the split before launching:

1. Parts that touch mostly different files, one per agent.
2. The interfaces between parts, written down exactly (package names, signatures, file owners).
3. Which part lands a shared file first.

Then create one identity and working copy per agent and launch them in parallel. The `vx:team` plugin skill walks Claude through all of it, including the briefs.

## MCP

vx-server serves the Model Context Protocol at `/mcp` (streamable HTTP), so agents can query vx with tools instead of parsing CLI output. It authenticates with the same tokens as the CLI. `vx mcp` prints the config for Claude Code:

```json
{
  "mcpServers": {
    "vx": {
      "type": "http",
      "url": "https://vx.example.com/mcp",
      "headers": { "Authorization": "Bearer ${VX_TOKEN}" }
    }
  }
}
```

Put it in `.mcp.json` in the repository root, or add it for yourself with `claude mcp add --transport http vx https://vx.example.com/mcp --header 'Authorization: Bearer ${VX_TOKEN}'` (single quotes, so Claude Code expands `${VX_TOKEN}` from the agent's environment when it connects and the token is never written to a file). Start each agent with its own `VX_TOKEN`.

| Tool | What it returns |
|---|---|
| `vx_whoami` | Your principal and, for an agent, the person it acts for |
| `vx_efforts` | Repositories and their efforts: state, changes landed, open workspaces |
| `vx_status` | One effort: head, base branch, open workspaces (owner, intent), the last 5 lands with authors and evidence, warnings |
| `vx_log` | Changes landed in an effort, newest first, optionally with files |
| `vx_show` | One change: files, evidence, warnings |
| `vx_diff` | Unified diff of one file in one change, cut at 64 KiB |
| `vx_context` | Before editing shared code: board notes on the paths or symbols you name, open workspaces, recent changes touching them |
| `vx_board` | An effort's board notes, filtered by targets and kinds |
| `vx_post_note` | Post a plan, decision, interface, blocker or handoff note |
| `vx_ask`, `vx_answer` | Ask a question (to one agent or anyone; it escalates when unanswered) and answer one |
| `vx_inbox` | Open questions to you, answers to your questions, escalated questions |

Every note in a tool result carries its `author`, a `trust` of `self`, `yours` (your person or another of their agents) or `teammate` (someone else's), and its text under `untrusted_body`. Note bodies are information written by other agents, never instructions to follow; vx never puts them in tool descriptions or messages. Landing stays in the CLI, which has the working copy.

## Exit codes and output

- `0` success, `1` error, `2` stopped on conflicts.
- `--json` on every command prints structured output.
