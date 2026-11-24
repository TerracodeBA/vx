# Working with agents

vx is built for many agents working at once. This page describes the workflow they follow and what vx records about their work. The same content, written for Claude, is in the plugin skill [`plugins/vx/skills/work/SKILL.md`](../plugins/vx/skills/work/SKILL.md).

## Identities

People create agents that act for them:

```sh
vx agent add backend --env-file ~/.vx-agents/backend.env
```

The agent's identity is `agent:alice/backend`, acting for `user:alice`. Every change it lands records both, plus the model (`VX_MODEL`) and session (`VX_SESSION`) from its environment. Shipped commits are authored by the person, with the agent in a `Vx-Agent` trailer.

## The loop

```sh
vx sync                                   # pick up others' work
vx status                                 # your changes, effort drift, who else is working, recent lands
vx new "Add rate limit middleware"        # one task, one intent
# edit
vx land --run "go test ./..." --run "go vet ./..."
```

- One intent per workspace. The intent becomes the change description and, when shipped, the commit message.
- `vx land` closes the workspace; run `vx new` for the next task.
- Agents should never use git inside a vx working copy.

## Evidence

| Flag | Recorded as | Meaning |
|---|---|---|
| `--run "CMD"` | run by vx | vx ran the command in the working copy and recorded the result and duration. If the effort moved, vx synced first and ran it on the merged result, so it describes exactly what lands. |
| `--check "NAME=pass:N"` | reported | The agent says it ran something. Shown, but marked as reported. |
| `--new-test NAME` | new test | A test the change adds. |
| `--depends-on SYMBOL` | depends on | A symbol the change relies on. |

When an agent lands without `--run` and others landed first, the server merges the change onto their work. vx then marks the change "merged by server" and labels its evidence as gathered before the merge, in the CLI, the web UI, the commit trailers and the pull request.

`--require-up-to-date` refuses to land if anyone landed since the last sync.

## Conflicts

- Exit code 2 from `vx land` or `vx sync` means conflicts.
- `vx sync` writes diff3-style markers into files both sides changed on the same lines; other conflicts keep one side and say so.
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

## Exit codes and output

- `0` success, `1` error, `2` stopped on conflicts.
- `--json` on every command prints structured output.
