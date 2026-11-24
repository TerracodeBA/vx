---
name: work
description: "Work inside a vx effort as a coding agent - open a workspace, edit, run checks, land, sync and resolve conflicts with the vx CLI. Use when the current directory is a vx working copy (it has a .vx folder), or the user asks you to work on, land into, or pick up a task in a vx effort."
when_to_use: "land this; work on the effort; vx land; vx sync; there is a .vx directory; pick up the next task in vx"
allowed-tools: "Bash(vx status:*) Bash(vx diff:*) Bash(vx log:*) Bash(vx show:*) Bash(vx effort show:*) Bash(vx wait:*)"
---

# Working in a vx effort

A vx **effort** is a shared line of work. Several agents, often for several people, land small changes into it at the same time. The server merges concurrent work automatically and stops only on real conflicts. Git stays the source of truth: the effort is shipped later as a pull request.

The working copy is a normal directory with a `.vx` folder. **Do not use git commands in it.** Use `vx`.

## The loop

1. **Look around.**
   ```sh
   vx status          # your changes, whether the effort moved on, who else is working, recent lands
   vx log --files -n 10
   ```
   Read what others are doing ("Others working in this effort") before you start, and avoid editing the same functions as an open workspace if you can.

2. **Start one task.** One intent per workspace, small and coherent. The intent becomes the change description and the commit message.
   ```sh
   vx sync
   vx new "Add rate limit middleware for POST /api/login"
   ```

3. **Edit, then land with evidence vx runs itself.**
   ```sh
   vx land --run "go test ./..." --run "go vet ./..."
   ```
   Use the project's real test and lint commands. With `--run`, vx syncs first if the effort moved, runs the commands on exactly what will land, and lands only if they pass. Prefer it over `--check`, which records a result you report yourself and is marked "reported".

4. **Repeat** from step 2 for the next task. `vx land` closes the workspace.

## When things collide

- **Exit code 2 from `vx land` or `vx sync` means conflicts.** Run `vx sync`. Files that both sides changed on the same lines get markers:
  ```
  <<<<<<< local
  your version
  ||||||| base ...
  original
  =======
  their version
  >>>>>>> effort ...
  ```
  Resolve them by understanding both changes (read `vx log --files` and `vx show <change-id>` for theirs), run the tests, then `vx land` again. `vx land` refuses files that still contain markers.
- **The text merged but the code broke** (for example both sides added the same helper): for Go, `vx land` prints `Warning: duplicate func clientIP in internal/httpapi (audit.go, limit.go)` when a package declares a top-level name twice; the change has landed anyway. Other breakage vx cannot see; your `--run` checks catch it. Fix it in a new workspace and land.
- **Waiting for someone else's change:** `vx wait --timeout 10m && vx sync` instead of polling in a loop.

## Useful flags

- `--json` on any command for structured output.
- `vx land -m "intent"` opens a workspace and lands in one step, for a quick change.
- `vx land --require-up-to-date` refuses to land if anyone landed since your last sync.
- `vx land --new-test TestName` and `--depends-on pkg.Symbol` record extra context for reviewers.
- `VX_MODEL` and `VX_SESSION` environment variables are recorded with each change.

## Rules

- Never land files with conflict markers or failing checks.
- Never edit `.vx/` by hand.
- Do not ship (`vx ship open`) unless the user asked you to; shipping pushes a branch to the git host.
- Keep intents factual: what the change does, not how hard it was.
