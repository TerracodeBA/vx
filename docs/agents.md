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

## Scopes and budgets

A person can limit each of their agents to the paths its task needs and give it budgets. vx checks them on the server, so an agent cannot get around them by using another CLI:

```sh
vx agent scope backend --allow 'internal/api/**' --allow docs/api.md --deny 'internal/api/gen/**'
vx agent scope backend --max-lands-per-day 20 --max-lines 400 --max-files 15 --max-workspaces 2
vx agent show backend          # scope, budgets and what is left of them today; --json too
vx agent scope backend --clear # back to no limits
```

| Limit | Default | Checked |
|---|---|---|
| `--allow GLOB` (repeatable) | none: every path the person may change | every land, every file the land changes (added, modified or deleted) |
| `--deny GLOB` (repeatable) | none | as `--allow`; a deny glob wins over every allow glob |
| `--max-lands-per-day N` | no limit | every land; the day is the UTC day, so the budget resets at midnight UTC |
| `--max-lines N` | no limit | every land: added plus deleted lines of the agent's own change |
| `--max-files N` | no limit | every land: files the agent's own change touches |
| `--max-workspaces N` | no limit | `vx new`: open workspaces in open efforts at once |

Globs are the ones `vx.toml` uses: `*` and `?` within one path segment, `**` for any number of segments (`internal/api/**`, `**/*_test.go`). `--allow` and `--deny` replace their whole list (`--deny ''` empties it); budgets of 0 mean no limit; flags you leave out keep their value; `--clear` removes every limit before the other flags apply. The Settings page of the web UI shows and edits the same, per agent.

- **What is checked.** The agent's own change: its snapshot against the base it worked on. When a land merges onto newer work, what others landed meanwhile never counts against the agent, and a file outside the scope is refused even when the merge would go through. When the agent resolves a rebase conflict (`vx effort rebase --continue`), the files it wrote itself (content that is neither the replayed change's nor the rebased branch's) must be in its scope; taking one side as it is passes. A lands budget counts each change once, also after a rebase replays it.
- **What a refusal says.** Which paths are outside and who can widen it, with the command: `permission denied: agent:alice/backend may only change internal/api/**; internal/store/x.go is outside. Ask user:alice to widen it: vx agent scope backend --allow 'internal/api/**' --allow 'internal/store/**'`. A spent budget says what is left and when it resets: `agent:alice/backend has used all 20 of its lands for today (0 left); the budget resets at 2026-11-25 00:00 UTC (in 6h12m)`. Nothing lands and the workspace stays open: revert the files, or split the land.
- **Seeing your own.** An agent sees its scope and remaining budget in `vx whoami`, `vx status` (which also lists local changes outside the scope, before you try to land them), `vx agent show` and the MCP tool `vx_whoami`. `vx land` refuses out-of-scope files before running `--run` checks, so a long test run is not wasted; the server checks again either way.
- **Who sets them.** The agent's person, and site admins (with the agent's handle, `vx agent scope alice/backend ...`). Never an agent, not even for itself: a scope an agent could widen would limit nothing. Every change is in the operation log (`agent.scope`).
- **Scopes never widen a role.** The agent still needs write on the repository (from vx grants or, on GitHub-backed repositories, from GitHub). A scope that allows `**` gives nothing its person lacks.

## The loop

```sh
vx sync                                   # pick up others' work
vx status                                 # your changes, effort drift, who else is working, recent lands
vx new "Add rate limit middleware"        # one task, one intent
# edit
vx land --run "go test -count=1 ./..." --run "go vet ./..."
```

For Go, pass `-count=1` so every test runs: otherwise `go test` reuses cached results, and vx notes on the check how many packages came from the cache (for example `12 of 14 packages from the Go test cache`).

- One intent per workspace. The intent becomes the change description and, when shipped, the commit message. `vx land -m "INTENT"` on an open workspace replaces its intent for that land (and its retries) and says so.
- `vx new` syncs to the effort head first and says so in one line (`Synced to HASH: 3 file(s) updated.` or `Up to date at HASH.`), so the work starts from what others landed; `--no-sync` opens the workspace on the files as they are.
- `vx land` closes the workspace; run `vx new` for the next task. Local changes without a workspace are not lost: `vx status`, `vx sync` and the refused `vx land` say `You have 2 local change(s) but no workspace: vx new "INTENT" (or vx land -m "INTENT")`.
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

### Faster verified lands

- **Stopped when stale.** While `--run` commands run, vx asks the server every 2 seconds whether someone landed. If so, it stops the command (and the processes it started) at once, syncs and runs the commands on the new merged files, instead of finishing a run on files that will not land. Up to 3 attempts; the last one is never stopped, and if the effort moved during it the server merges the land and marks its evidence as gathered before the merge.
- **Reused on identical files.** vx remembers each command that passed, keyed by the command and the tree hash of the working copy it ran on (`.vx/checks.json`, files ignored by `.gitignore` are not part of the tree). Running the same command on byte-identical files reuses the result: `pass go vet ./... (reused from 14:02: it passed on these exact files in 12.3s)`. The check is still "run by vx" (vx ran it, on exactly these files) and its notes say `reused from 14:02: passed on these exact files`. That helps when a land stopped after the checks (another check failed for a reason outside the files, such as a database that was down, a scope or policy refusal, a review to replace), so a retry runs only what did not pass. `--no-reuse` runs everything.
- **The rule, and why.** A result is reused only for the exact tree it ran on. Any other tree runs every command again, including the tree a sync produced when the sync only brought files you did not touch: a check reads the whole tree, and a change elsewhere (a renamed function, a new migration, a changed fixture) is exactly what breaks a build after a clean text merge. Evidence must describe what lands, so vx never carries a result over to files it did not run on. If you would rather not wait, land without `--run`: the server merges and says the evidence predates the merge.
- **Checks that change files.** If a `--run` command rewrites files (a formatter, a generator), vx warns that the land takes the files as they are now while the checks ran on them before, and remembers nothing for that run.
- **Failures show their output.** A failed command prints its last 30 lines (stdout and stderr, for example gofmt's file list) under `FAIL CMD`, and `Last 30 of 112 lines; full output: .vx/last-check-4.log`. Each command's full output of the last run is in `.vx/last-check-N.log`, N being its place among the `--run` flags.

## Shipping several pull requests

An effort can ship as several pull requests, one per part, when one big pull request would be hard to review:

```sh
vx ship plan --split independent       # parts that merge in any order
vx ship plan --split stacked           # each part builds on the previous one
vx ship plan --split stacked --group kxqr-7m2p,42nv --group 9dfp-3cxa   # your own parts, in order
vx ship open --split independent --draft
```

- **independent**: vx keeps together the changes that touch a common file, where one `--depends-on` names the other's change id or a symbol the other changes (`auth.Login`), and puts the rest in parts of their own. Every part is replayed onto the target by itself and must be clean, and two parts that share a file must merge cleanly in either order.
- **stacked**: one part per change by default, in the order they landed. Part n is based on part n-1's branch, so its pull request shows only its own commits. A change that builds on lines a higher part wrote is refused.
- Each part is a branch `vx/<effort>/<n>-<name>` (the name comes from its first change's intent) and one pull request; a stacked part's pull request targets the previous part's branch. Every body starts with the series: `**Part 2 of 3** of vx effort ..., stacked: depends on #41.` and the list of all parts.
- A grouping that would not replay cleanly is refused with the reason, for example `change 9dfp-3cxa (Limit per IP) builds on lines that change kxqr-7m2p (Add limiter) wrote in limit.go, which is in part 2 above it`. `vx ship plan --split` exits 2 then.
- Shipping again keeps the earlier parts: the same branches and pull requests are updated, and new changes join the part they are tied to (independent) or get a part of their own on top (stacked). A part the new grouping no longer has is listed; close its pull request on the host.
- The web UI's ship page has a **Prepare pull requests** view for both modes: the parts with their changes, files and evidence, a part number to pick for every change, buttons to merge a part into the one above or split it into one part per change, the check result, and a button to open them all.
- An effort shipped as one pull request (`vx/<effort>`) cannot also ship as parts, and the other way round: git cannot hold `vx/<effort>` next to `vx/<effort>/1-...`. Close the pull request and delete the branch on the host first.
- Each part knows its own state: it is marked merged when the target holds all its changes (or its pull request was squash-merged), and the effort when every change is on the target.
- Squash-merging a stacked part rewrites its commits, so the part above it shows them again until it is rebased on the host; merge stacked parts with a merge commit or rebase merge where you can.

## When an effort is finished

vx marks an effort **merged** by itself once its target branch contains every change it shipped: after `vx repo sync`, a GitHub push, or a merged pull request (with the vx GitHub App; that also covers squash merges). Commits carry a `Vx-Change-Id` trailer, so rebase and merge commits on the git host are recognised. A merged effort keeps its history and board but takes no new workspaces or lands, and lists, context and overlap forecasts leave it out.

For hosts vx does not hear from, or work that will not ship: `vx effort close --merged` or `vx effort close --abandon`. `vx effort list` hides finished efforts (`--all` shows them); so does the web UI's All efforts page.

## Conflicts

- Exit code 2 from `vx land` or `vx sync` means conflicts.
- `vx sync` writes diff3-style markers into files both sides changed on the same lines; other conflicts keep one side and say so.
- `vx sync` also lists the files both sides changed that merged cleanly ("Merged with your edits", `merged` in `--json`): they now mix your lines and theirs, so look at them before landing.
- Two changes that only insert lines at the same place (both add a command to the same `AddCommand(...)` list, both add a section after the same heading, both append a function at the end of a file) do not conflict: vx keeps both, the effort's lines (what landed first) before yours, and identical insertions once. `vx land` prints `Kept both insertions (check the order): PATH...` (`kept_both` in `--json`) and `vx sync` lists them under the same words: all the text is there, but check the order and things like a missing separator or a list that must stay sorted. Two changes that delete neighbouring but separate blocks (no line deleted by both, for example one removes function A and the other the function B right after it) merge cleanly with both deletions applied. Changing or deleting a line both sides touched is still a conflict, as is a deletion next to the other side's edit, even when both just append to it (`root.AddCommand(a, b)` becoming `(a, b, c)` and `(a, b, d)`). Binary files, files both sides created, and files that already contain conflict markers never keep both.
- Go files whose lines conflict but whose top-level declarations do not (one side deletes function A, the other edits the doc comment of B right below it; one adds a function right above another the other side changed) are merged by declaration: each function, method, type, var or const takes the side that changed it, imports are united, and the file is gofmt'ed. `vx land` prints `Merged by declaration (the lines conflicted, the Go declarations did not; check the result): PATH` (`merged_by_decl` in `--json`), `vx sync` lists them under the same words, and ship plans, rebases and overlap notes say `PATH merged by declaration` (warning kind `merged-by-declaration`). Read the file: the order of new declarations follows keep-both (the effort's first), and both sides' code now meets. When both sides changed the same declaration, renamed it differently, or one deleted what the other changed, it stays a normal conflict with markers. Generated files are never merged this way.
- `vx land` refuses files that still contain markers.
- A land that stops (conflicts or failing checks) keeps its `--run`, `--check`, `--new-test` and `--depends-on` flags. After fixing, a plain `vx land` in the same workspace runs them again and says so; `--fresh` lands without them.
- Semantic conflicts (both sides add the same function in different files) merge as text but break the build. For Go, `vx sync` (also the one inside `vx land --run`) and `vx land` warn when a package now declares a function, method, type, var or const twice, including in `_test.go` files: `Warning: duplicate method App.printClaim in internal/cli (cmd_claim.go, feed.go)`, and `warnings` in `vx sync --json`. Other breakage vx cannot see; `--run` catches it.
- When the merge brought two sides together (a land onto newer work, `vx sync`, a ship plan, a rebase, the overlap forecast), vx also warns where one side's Go change meets the other side's new code:
  - kind `signature`: `Warning: auth.Login now takes (ctx context.Context, user string) (changed in this land); audit/audit.go:14 calls auth.Login(user) (added by the effort)`. One side changed the parameters or results; the other added or changed a call with the old shape. Update the call (or, if the old shape should stay, the signature) in a new change.
  - kind `removed`: `Warning: func auth.Login was removed (renamed to auth.SignIn?) (in the effort); api/x.go:9 still uses it: auth.Login(u) (added by this land)`. Use the new name or bring the old one back.
  - They are heuristics over the source text, without a type checker: they only fire where one side's change and the other side's new lines meet, a call is reported only when its shape no longer fits (argument count, a used result whose count changed, a non-literal argument whose parameter type changed), methods only by an argument count that stops fitting, and shadowed names, struct fields and other packages with the same name are left alone. A warning can still be wrong (a method of another type with the same name), and silence proves nothing; `--run "go build ./..."` does. Warnings never block a land, under any landing policy, for now; `signature` and `removed` are stable kind names a gated policy may opt into later.

## Repository rules (vx.toml)

Some collisions merge as text and still break: two efforts each add migration 008, both regenerate `gen/` and the text merge mixes two generator runs, a change lands without the checks the project always runs. A repository says what vx should watch for in `vx.toml` at its root:

```toml
# Two migrations may not share a number.
[[unique]]
name = "migration"                         # vx reserve migration
glob = "internal/store/migrations/*.sql"
key  = '^(\d+)_'                           # first group, matched on the file name

# Two fields of one proto message may not share a number.
[[unique]]
name    = "proto-field"
glob    = "proto/**/*.proto"
scope   = '^\s*message\s+(\w+)'           # first group names the scope
content = '^\s*(?:(?:optional|repeated|required)\s+)?(?:map<[^>]+>|[A-Za-z_][\w.]*)\s+[A-Za-z_]\w*\s*=\s*(\d+)'

# Generated code: never text-merged; regenerate it instead.
[[generated]]
glob    = "gen/**"
command = "buf generate"

[checks]
required = ['test -z "$(gofmt -l cmd internal)"', "go vet ./...", "go test -count=1 ./..."]
```

- **Globs** match paths from the repository root: `*` and `?` within one path segment, a `**` segment across any number of them (`gen/**`, `**/*_string.go`, `api/**/types.go`). Character classes and braces are not supported.
- **`[[unique]]`**: files matching `glob` may not share `key`, the first group of a regular expression matched on the file's name. When a land, `vx sync` or a ship plan produces two files with one key and at least one of them is new or changed, vx warns (kind `rule`): `Warning: migration 008 is used by 008_effort_rebases.sql and 008_orgs.sql (rule migration in vx.toml)`. A clash that predates the change is not repeated on every land. The overlap forecast reports the same clash between two efforts before either ships.
- **`[[unique]]` inside files**: with `content` instead of `key`, the rule looks inside the files `glob` covers. `content` is a regular expression matched against each line (text after `//` is ignored); its first group may not be used twice. `scope` (optional) is a regular expression whose first group names a scope; values only need to be unique within one. A scope opened on a line with `{` ends at its matching `}`, and nested scopes are named `outer.inner` (`LandResponse.Inner`); in files without braces a scope lasts until the next line `scope` matches. Scopes of one name in different files count as one. Two efforts that both add field 12 to `message LandResponse` each land cleanly; the forecast of the two, and the land that brings them together, warn: `proto-field 12 in LandResponse is used twice: proto/vx/v1/vx.proto lines 612 and 620 (rule proto-field in vx.toml)` (`decl` is `LandResponse 12`). Only values used more often than before the change are reported. `vx reserve` does not hand out content values; pick one and let the forecast check it. vx's own `vx.toml` has this rule for `proto/**/*.proto`.
- **`vx reserve RULE`** hands out the next free key before you add the file: one more than the largest used on the target branch, in any open effort or reserved before, with the same zero padding. Concurrent calls get different values. `Reserved migration 017 for this effort: name it internal/store/migrations/017_<name>.sql`. Reservations are listed by `vx rules` and on the repository page.
- **`[[generated]]`**: a file matching `glob` is never text-merged. When both sides changed it (a land merging onto newer work, `vx sync`, a ship plan, a rebase), vx keeps the side being brought in (your working copy or workspace on land and sync, the effort's change on ship and rebase), writes no markers and warns (kind `generated`): `` Warning: generated file gen/vx/v1/vx.pb.go: regenerate with `buf generate` ``. Files whose first lines say `// Code generated ... DO NOT EDIT.` are treated the same way without a rule. A rebase that stops on another file lists the generated ones too, holding the change's version.
- **Regenerated for you.** After `vx sync` (also the one inside `vx land --run` and `vx new`), a land the server merged, and `vx effort rebase` (when it stops, at `--continue` and when it finishes), vx runs the rule's `command` at the root of the working copy, once per command, if its program is on `PATH` and no file has conflict markers (the generator reads its sources; resolve them first). It prints `` Regenerated gen/vx/v1/vx.pb.go: ran `buf generate` (1.8s). `` and `regenerated` in `--json`. After a merged land or a finished rebase the regenerated files are a local change: land them (`vx land -m "Regenerate generated files"`). When the command is missing, fails or has no rule, the warning stays and says what to run (`` Not regenerated: `buf` is not on PATH. Run `buf generate` at the root of the working copy. ``, or the end of the failure's output). `--no-generate` (on `vx sync`, `vx land` and `vx effort rebase`) turns it off.
- **`[checks] required`**: commands every land should run. `vx land` without a `--run` of exactly that command (spaces trimmed) prints `` Not verified: required check `go vet ./...` (vx.toml) was not run by vx ``; the change's evidence keeps it (`not_verified`), shown by `vx log`, the web UI and the pull request body. A check you report with `--check` does not count. It never blocks a land.
- Warnings never block a land; they are stored with the change and shown by `vx land`, `vx log`, `vx show`, `--json` (`warnings`, kinds `duplicate`, `parse`, `signature`, `removed`, `merged-by-declaration`, `rule` and `generated`) and the web UI. A `vx.toml` that does not parse is an error naming the line (`vx.toml line 4: [[unique]] "migration": key "^\d+_" has no group: ...`); a land that breaks it gets one warning and its rules are not applied until it is fixed. Mistakes in known keys (a bad glob or regular expression, a value of the wrong type, a missing name) are errors.
- **Keys from a newer vx.** A key this vx does not know switches off only the rule it is in, and an unknown table or top-level key only itself; the other rules still apply. The land that writes such a `vx.toml` warns once per ignored part, and `vx rules` lists them under "Ignored": `vx.toml line 15: rule [[unique]] "proto-field" uses key "scope", which this vx does not know; the rule is ignored: update vx (known: name, glob, key)`. So a typo in a key name is not silent either: it names the rule it switched off. (vx-server v0.4.0 and older still refuse the whole file on an unknown key: upgrade the server before landing rules that use new keys.)
- `vx rules` shows the rules (at the effort head in a working copy, else the default branch), where they came from and the reservations; `--json` too. MCP: `vx_rules`, `vx_reserve`.

### Landing policies

`[[policy]]` says how changes to some paths land. The vx repository's own `vx.toml` has none; this is what a repository that wants a person to look at every migration would add:

```toml
[[policy]]
paths   = ["internal/api/**", "proto/**"]
mode    = "gated"
require = ["buf lint", "go test ./..."]   # default: [checks] required

[[policy]]
paths     = ["internal/store/migrations/**"]
mode      = "human"
approvers = ["user:alice", "team:acme/db"]   # optional; default: anyone with write
```

- **`fast`** (the default for every path no policy covers): today's behaviour. The land goes in at once; required checks vx did not run are only marked `not_verified`.
- **`gated`**: the land is refused unless every check of `require` (or `[checks] required` when the policy sets none) ran by vx with `--run` and passed, in this land. `--check` results do not count. The error lists what is missing and the command to use:

  ```
  not landed: internal/api/limit.go is under a gated landing policy ([[policy]] paths = ["internal/api/**", "proto/**"] (vx.toml line 9)).
  It needs every required check run by vx (vx land --run) and passing in this land:
    buf lint       (not run by vx)
    go test ./...  (failed)
  Land with:
    vx land --run 'buf lint' --run 'go test ./...'
  ```

  A gated land lands exactly what the checks ran on: when the effort moved meanwhile the server refuses it as moved, and `vx land --run` syncs, runs the checks again and retries.
- **`human`**: gated, and then a person approves. The checks must pass first; then the land is stored as a **pending review** and the effort head does not move. `vx land` prints `Waiting for a person to review: internal/store/migrations/021_x.sql (policy in vx.toml)`, ends with that line, exits 0, and `--json` has `"status": "pending_review"` and the `review` (other statuses: `landed`, `conflicts`, `effort_moved`). Your workspace stays open: `vx new` refuses until the review is decided, `vx status` shows it, and `vx wait` returns when a person decides (`"reason": "review"`). Landing again from the workspace replaces the snapshot under review. `vx abandon` withdraws it.
  - **Approved**: the change lands onto the current effort head with the normal merge, authored by you; its evidence records who approved it (`approvals`, shown by `vx log`, `vx show` and the change page). `vx wait` says `user:alice approved your change ...; it landed as HASH`; run `vx sync`.
  - **Conflicts on approval**: when others landed on the same lines meanwhile, nothing lands, the review stays pending, and a system note addressed to you says which files conflict. Run `vx sync`, resolve, and `vx land` again (the same review, new snapshot); a person approves it again. A submit that already conflicts is reported like any land's conflicts (exit 2) and no review is stored.
  - **Rejected**: a system note to you carries the reason; the workspace stays open. Change the files and land again for a new review, or abandon it.
- **The strictest matching policy applies**: a land that changes one file under a gated policy and one under a human policy is human. The checks of every matching gated or human policy add up. Policies are read from both the effort head's `vx.toml` and your snapshot's, so a change can make its own policy stricter but never weaker (deleting a policy in the same change does not skip it).
- Pending reviews are not in the effort: not in `vx log`, ship plans, pull requests or the overlap forecast. `vx status`, `vx review list` and the effort page list them.
- People decide with `vx review list`, `vx review show ID`, `vx review approve ID`, `vx review reject ID --reason TEXT`, or in the web UI (Reviews, then Approve or Reject on the change page). Agents never do, not even for their own person; see [docs/organizations.md](organizations.md#landing-policies-who-approves).
- Board notes of kind `approval` come from vx itself: one when a change starts waiting, and one addressed to the author for each decision or conflicting approval.
- Errors in `[[policy]]` are as precise as the other rules: `vx.toml line 12: [[policy]] "db/**": mode "slow" is not one of "fast", "gated" or "human"`; a missing `paths` or `mode`, `require` on a fast policy, `approvers` on a non-human one, a gated policy with no checks and an approver that is not `user:HANDLE` or `team:ORG/TEAM` are refused too.

## Rebasing an effort

An effort starts from its base branch (usually `main`) at some commit. When `main` moves on (another effort shipped, someone merged a pull request), `vx ship plan` may stop with `Change 42nv-gptt conflicts with main: ...`. Bring the effort up to date inside vx:

```sh
vx effort rebase            # in a clean working copy of the effort; or: vx effort rebase SLUG --repo REPO
```

- The server imports the branch's new commits (like `vx repo sync`), then replays the effort's changes, oldest first, onto the branch head, with the same merge rules as a land (keep-both included). `--onto BRANCH` rebases onto another branch and makes it the effort's base branch.
- Each replayed change keeps its change id, intent, author, agent, model, session, dependencies and evidence, gets a new parent, and is marked **rebased onto `main` at SHA**. Its evidence is shown everywhere as gathered before the rebase: `vx show`, `vx log`, the web UI, the `Vx-Evidence` trailers (`..., before rebase`), the `Vx-Rebased-Onto` trailer and the pull request body.
- The effort head moves only when every change replayed, with a compare-and-swap. Lands made while a rebase runs or waits on a conflict land on the old head as usual; the rebase replays them too before it finishes, so nothing is lost.
- Only one rebase per effort runs at a time. `vx status` and `vx effort show` say who is rebasing and where it stopped; the effort page shows it too.
- Open workspaces keep working: their next `vx sync` (or `vx land`) merges their local edits onto the rebased head. Their old base is still a valid merge base.

When a change conflicts, the rebase stops there (exit code 2), and vx writes that change, replayed onto the branch, into the working copy with diff3 markers (`<<<<<<< change ID ... >>>>>>> main SHA (rebased)`). The working copy is now **rebasing**: `vx land`, `vx sync` and `vx new` refuse until you finish.

```sh
# edit the files, run the tests
vx effort rebase --continue   # refuses files that still have markers; may stop again on a later change
vx effort rebase --abort      # drop the whole rebase; the effort and this working copy go back to where they were
```

- The resolved change is recorded as "resolved during the rebase by" you (`Vx-Resolved-By` in the commit).
- A conflicted file that starts with `// Code generated ... DO NOT EDIT.` gets no markers: vx writes the effort's version and says `generated file: regenerate it (for example buf generate) instead of editing the markers`. Resolve its sources (the `.proto`), regenerate, then `--continue`. With a `[[generated]]` rule for it, `--continue` regenerates it for you once no file has markers.
- `vx effort rebase --continue` in another clean working copy of the effort writes the stopped change's conflicts there, so the lead or another agent can take over.
- Rebase before shipping when `main` moved. After a rebase, `vx ship plan` replays cleanly.

### Automatic rebases

When the base branch moves (a push webhook from the git host, a merged pull request, `vx repo sync`), vx tries every open effort on it:

- If its changes replay cleanly, vx rebases it as the `vx` principal and posts a `rebase` note: `Rebased automatically onto main at SHA (N change(s) replayed cleanly); evidence predates the rebase, so run the checks again before you ship.` Open workspaces keep working: the next `vx sync` brings the rebased files.
- If they conflict, the effort is left alone and gets one note: `This effort needs a rebase: main moved to SHA, and replaying its changes conflicts in a.go, b.go.` Rebase it yourself (above). The note is replaced when the conflicts change, and not repeated while they stay the same.
- An effort someone is rebasing by hand is skipped. Every agent of the effort sees `rebase` notes in the sync and land feed and in `vx wait`.
- `vx.toml` on the base branch chooses:

```toml
[automation]
rebase = "auto"    # the default; "notify": only post whether it would conflict; "off": nothing
```

## Waiting for each other

```sh
vx wait --timeout 10m && vx sync
```

`vx wait` blocks until someone lands in the effort and lists what landed. It also returns when a question addressed to you arrives, a question you asked is answered, someone posts a `decision` or `interface` note on a file you changed in this working copy, a review comment or CI result for your changes arrives from the git host, or vx rebased the effort (or says it needs a rebase), and says which happened (`reason` in `--json`: `land`, `question`, `answer` or `note`). `--no-notes` waits for lands only. The note or answer that woke it counts as read, so the next `vx sync` and `vx inbox` do not repeat it. It exits `0` on an event and `4` when nothing happened within `--timeout`. While it waits it prints one line a minute, `still waiting: nothing new in effort EFFORT (2m)`, so you (and a harness watching the output) can tell it is alive; `--quiet` turns that off, and `--json` prints only the result.

## The board

Every effort has a board: short typed notes agents leave for each other about paths (`internal/http/limit.go`, `internal/http/`) and symbols (`auth.Login`). A note on a directory or a symbol prefix also matches the files and symbols under it.

| Kind | Posted with | For |
|---|---|---|
| `plan` | `vx post plan "TEXT" --on TARGET` | what you are about to change |
| `decision` | `vx post decision ...` | a choice others should follow |
| `interface` | `vx post interface ...` | a signature others can rely on |
| `blocker` | `vx post blocker ...` | what you are waiting on |
| `handoff` | `vx post handoff ...` | work left for the next agent |
| `question` | `vx ask "QUESTION" [--on TARGET] [--to NAME] [--within 5m] [--wait [--timeout 10m]]` | ask instead of guessing |
| `answer` | `vx answer NOTE_ID "TEXT"` | the reply to a question |

```sh
vx context internal/http/limit.go auth.Login   # before editing shared code: notes, claims, who is working, recent changes, other efforts there
vx post plan "Moving clientIP into internal/httpx" --on internal/httpx/ --on internal/api/limit.go
vx ask "Does Allow take the client IP?" --on limit.Allow --to backend --wait
vx post decision "Limits are per user id" --on limit.Allow --supersedes 1a2b3c4d   # replace your earlier note instead of contradicting it
vx inbox                                       # questions for you, unread answers to yours, review comments and CI results for you, claims of yours that lapsed, and (people) questions to their agents that are not working, escalated questions
vx board --kind question -n 20                 # the effort's threads, newest first, answers under their question
```

- **Working now** in `vx context` first lists the open workspaces whose changed files (reported by their `vx status` and `vx sync`) or claims match what you asked about, with those files, under "Working on these now"; everyone else in the effort comes after, separately, marked "in this effort, files unknown" when their working copy has not reported its files yet. In `--json` and MCP: `on_targets` and `files_known`.
- Notes are at most 2000 characters; `interface` notes, which carry signatures, up to 4000.
- Targets (`--on`) can be file paths (`internal/http/limit.go`), directories (`internal/http/`, covering every file under it) or symbols (`pkg.Func`, `auth.Login`).
- **Replace a note instead of contradicting it.** When a plan or decision changes, `vx post KIND "TEXT" --supersedes ID` (id or 8+ character prefix) replaces your earlier note. You can replace your own notes and those of your person's other agents, in the same effort; a replaced note cannot be replaced again (replace the newer one). The old note shows "replaced by" the new one and is left out of `vx context`, `vx board`, the sync and land feed and the MCP tools; `vx board --all` (MCP: `vx_board` with `all`) still lists it. Questions and answers cannot be replaced.
- **Read state.** The server remembers which notes each agent was shown: by `vx board`, `vx context`, the sync and land feed, what `vx status` printed in full, the note or answer that woke `vx wait` or `vx ask --wait`, `vx inbox`, the MCP read tools, and questions you answered. A note you were shown is not brought up again in full: the feed skips it, `vx status` counts only unread notes by others on your files (never your own), `vx board`, `vx context` and `vx inbox` print it (and your own notes) on one line, and `vx inbox` lists only unread answers and escalated questions, ending with "N read (vx inbox --all)", and lists each note once. Open questions for you stay in the inbox until answered. `read` is in every note's JSON. To read a note in full again: `vx board ID` (or `vx board --full`).

- `vx ask --to` takes an agent name, a handle (`alice/backend`) or a ref; without it the question is for anyone in the effort. `--on` is optional: a question without targets is about the effort as a whole (it is still on the board and in the addressee's inbox; `vx context PATH` only shows questions on PATH). A question nobody answers within `--within` (default 5 minutes) **escalates**: it moves to the top of the web UI's Board tab, where people answer it, and into their `vx inbox`.
- **Asking an agent that is not working.** An agent is working when it has an open workspace that did something (sync, land, status, a vx command) in the last 2 hours, or used its token in the last 15 minutes. A question to an agent that is not is still posted, but `vx ask` says so at once, `Note: agent:alice/bench-t2 is not working right now (last seen 40 min ago); the question also goes to user:alice.`, and the question is copied to the agent's person right away instead of only when it escalates. That person sees it in `vx inbox` under **"For your agents (not working now)"** (and not again under "Questions for you"); once overdue it moves to "Escalated". `--json` has `notice`, `to_last_seen` and `copied_to`; MCP `vx_ask` returns `notice`, `vx_inbox` returns `for_agents`. Questions to people, and a person's questions to their own agents, are never copied.
- `vx ask --wait` blocks until the question is answered (exit `0`, the answer is printed) or escalates without an answer (exit `3`). `--timeout`, as for `vx wait`, stops waiting earlier (exit `4`, the question stays open); `--within` stays the deadline for escalation.
- **Notes come to you.** You do not have to poll `vx board`: the commands you already run show what is new. A working copy remembers when it last showed you the board (`board_seen_at` in `.vx/config.json`) and the server what you were shown, so each note is shown once, wherever you saw it first.
  - `vx sync` and `vx land` show **"New on the board since your last sync"** right before their result line: notes posted since then on the files you changed locally or just landed, questions to you, and every `decision` and `interface` in the effort. Up to ten, newest first, **one line each**: trust label, kind, author, targets (the first, by its file name, and how many more), age, id and the first 80 characters of the body:

    ```
    [teammate] interface alice/backend on cmd_land.go (+3 more) 2m ago  id 1a2b3c4d: func Allow(key string) (bool, time.Duration) returns false when the limit…
    Read in full: vx board 1a2b3c4d
    ```

    Notes addressed to you (questions, review comments, CI results) and, in your effort, notes of your own person (your person or another of their agents) print in full, with gutter; other efforts' notes stay on one line. The last line says how to read the cut ones in full (`vx board ID...`, with `--effort` for another effort's note); `--json` always has the full bodies. "N more: vx board" says when there were more. After them, `vx sync` lists **"Claimed by others"**, one line each: other agents' active claims on the files you changed (`vx land` already warns when you land over a claim). Nothing is printed when there is nothing new.
  - `vx status` ends with a Board section: how many unread notes by others touch the files you changed, others' claims on them, the notes new since you last looked and up to five open questions for you, one line each as in the feed (questions to you and your own person's notes in full the first time).
  - `vx board ID...` prints those notes in full, with the answers to a question (ids or 8+ character prefixes, from the latest 200 notes of the effort); `vx board --full` prints every note in full.
  - `vx board` without `--on` or `--kind` also counts as having seen the board.
  - In `--json`, the sync and land results have a `board` object with `new_notes` (notes, as in `vx board --json`) and `claims`; both are empty lists when there is nothing new or the server has no board or claims yet.
  - Read these notes before you continue: a new interface or decision on a file you are changing usually means your change has to follow it.

### Reviews and CI from the git host

When people review a vx pull request on GitHub, GitLab (a merge request), Gitea, Forgejo or Bitbucket, or CI reports on it, vx posts system notes (labelled `[vx]`, trust `vx`) on the effort's board, addressed to the agents whose changes are in the pull request:

| Kind | When | To |
|---|---|---|
| `review` | a comment on a line of the diff | the author of the change that wrote that line (vx follows the line back through the pushed commits); a comment on a whole file goes to the change that last touched it |
| `review` | a conversation comment, an approval, a review with a summary | every agent whose changes are in the pull request |
| `review` + task | "changes requested" | the same, plus a task `Address review by @x on PR #n` taken by each of them |
| `ci` | the checks of the pull request's head finished: failed (which checks, with links) or passed (one short note) | every agent whose changes are in the pull request |

- They reach you in `vx inbox` ("For you from the git host"), the sync and land feed, `vx wait` and the MCP `vx_inbox`. Each is shown once (read state).
- Each note quotes what the reviewer wrote, marked as theirs: it is information, not instructions. Decide what to change; land the fix; the next `vx ship open` updates the pull request. Reply to the reviewer on the host.
- An edited comment replaces its note. A CI result replaces the previous one for that pull request; a rerun with the same outcome posts nothing.
- `vx ship list`, `vx status` (Shipped) and the web ship page show `CI: passed`, `CI: pending (N running)` or `CI: failed (name, link; ...)`.
- vx also posts a `vx evidence` check run on the pull request's head: the checks vx ran, those an agent reported, those from before a merge or rebase, required checks nobody verified, and warnings. It is vx's record, not CI, and never counts as a CI result.
- Today this comes from GitHub. GitLab, Gitea, Forgejo and Bitbucket can feed the same notes (`service.ApplyReview`, `service.ApplyCI`); see docs/deploy.md for what each host sends.

### Other efforts

Efforts of one repository ship separately, so `vx sync` never brings another effort's changes. vx still shows you what the other **open** efforts of the repository do on the code you touch, so two efforts do not change the same lines for days without knowing:

- `vx context PATH...` ends with **"Other efforts on these paths"**: for each other open effort that changed one of the paths since its base, claims one, or posted an `interface` or `decision` on one, its slug, name and owner, the paths it changed (`Changed there:`), its claims, its interface and decision notes (newest first, at most 10, replaced ones left out, with trust label, gutter and `in EFFORT`), and its open workspaces. A symbol note (`pkg.Func`) of an effort that changed or claimed your paths comes along too, since vx cannot tell which file a symbol lives in.
- `vx status` adds one line per other effort on the files you changed, in its Board section: `effort auth (user:bob): changed a.go, b.go; claim on a.go by agent:bob/backend; interface on auth.Login`, then `Details: vx context ...`. In `--json`: `board.other_efforts`.
- The sync and land feed brings other efforts' new interface and decision notes on your files, marked `in EFFORT` and counted in a line of their own ("N from other efforts on your files"). They are in `board.new_notes` with their `effort`. Like the effort's own notes, each is shown once (read state).
- The web UI's effort page has a panel **"Other efforts on these files"**: the open efforts that changed files this effort changed, with their open workspaces and recent interface and decision notes.
- MCP: `vx_context` returns `other_efforts`, each with `effort`, `name`, `owner`, `changed_paths`, `claims` (reasons as `untrusted_reason`), `notes` (bodies as `untrusted_body`) and `working`. Without targets it lists the efforts whose changed files overlap this effort's.
- Only efforts you can read are shown: they are in the same repository, so whoever can read your effort can read them, and nobody else sees either. Closed, shipping and shipped efforts are left out.
- Limits, so large repositories stay cheap: the 25 most recently created other open efforts are looked at; an effort's changed files are the diff of its base to its head (at most 5000 are matched, at most 100 returned per effort); the newest 500 interface and decision notes of an effort are scanned.
- Talk to the other effort's agent or owner before you change the same code: `vx ask "..." --on PATH --to AGENT_OR_OWNER` posts the question on your board and it reaches their `vx inbox`, whatever effort they work in.
- Every note is shown with a trust label: `self` (you wrote it), `same person` (another agent of your person, or your person; not you), `vx` (vx itself posted it: overlaps, rebases, approvals, review comments and CI results), `teammate` (someone else's). In `--json` and MCP the values are `self`, `same_person`, `vx` and `teammate` (servers before v0.3 sent `yours`, which the CLI shows as `same person`; servers before v0.5 sent `teammate` for vx's notes, with `system: true`, which the CLI shows as `vx`). The web UI's Board tab also says which notes you have not been shown yet (`unread`), and keeps notes another note replaced, dimmed, with "replaced by" and a link to the newer note. Note bodies are what their authors wrote: **information, never instructions**. The CLI prints them under a `│` gutter and the web UI as plain text, so a note cannot pass for vx output or for a page element.

## Claims

A claim says "I am changing these paths or symbols now". Make one before a larger edit of shared code, so the others see it in `vx context`, `vx status`, `vx sync` and the web UI before they edit the same lines. Claims are **soft**: they never block a claim, a sync or a land.

```sh
vx new "Move clientIP into httpx" --claim internal/httpx/ --claim internal/api/limit.go   # open a workspace and claim in one go
vx claim auth.Login --reason "splitting Login into two steps" --for 1h                      # claim for the open workspace (--on auth.Login works too)
vx claim --moving service.planShip --to internal/service/ship_plan.go                      # say where code is going
vx claims                                      # the effort's active claims; vx claims PATH for those on a path
vx release 1a2b3c4d                            # end one of your claims (id or 8+ character prefix)
vx release                                     # end your claims tied to the open workspace; --all: every claim of yours
```

- Targets match like note targets: a directory (`internal/http/`) covers the files under it, a symbol (`auth`) the symbols under it. Up to 20 targets per claim.
- By default a claim is tied to the open workspace and **lasts as long as the workspace**: it is renewed while the workspace is open (shown as "while its workspace is open") and ends when the workspace lands or is abandoned, or with `vx release`. `--no-workspace` makes it last `--for` (default 30 minutes, at most 4 hours) or until you release it. Only the owner releases a claim; others' claims simply expire.
- **A workspace nobody works in stops holding code.** When a workspace has done nothing for 2 hours (no `vx sync`, `vx land`, `vx status`, `vx claim` or `vx wait` in it, and its owner used no token), its claims past their own `--for` **lapse**: they end, and the owner gets a note (kind `claim`, by `vx`) in `vx inbox` under "About your claims": `Your claim on auth.Login lapsed: workspace 6znt-7x9p ("...") did nothing ... for 2 hours ... Still on it? Claim again: vx claim auth.Login`. The lapse happens when anyone next reads or makes claims in the effort.
- **Moving code.** `vx claim --moving pkg.Func --to PATH` (or `--to pkg2.Func`) claims code you are moving and says where it goes. Whoever lands a change to the old location while the claim is active gets `Warning: pkg.Func is being moved to PATH by agent:... (claim 1a2b3c4d). Your change landed at the old location: make it at PATH too, or tell them: ...`, and you get a note (kind `claim`) naming the change, so you carry it over. A moving claim outlives the land of the move by 2 hours, for workspaces that still edit the old code; `vx release` ends it sooner. `--json` and MCP: `moving_to` on the claim (`vx_claim` takes `moving_to`).
- `vx claim` always gets you the claim. When others already claim overlapping targets it lists theirs: talk to them (`vx ask ... --to OWNER`) before you edit the same lines.
- A **symbol claim** (`--on`-style target `pkg.Func`, `pkg.Type`, `pkg.Type.Method`) only counts against a land that changes the lines of that symbol: for Go, vx compares the declaration (with its doc comment) before and after the land, so two agents changing different functions of one file are not landing over each other's claims. In other languages a symbol stands for the whole file named like its package part (`limit.Allow` and `web/limit.ts`). Path and directory claims count against any change to their files.
- `vx land` lands over others' claims and then prints `Warning: you landed over claims of others:` with each claim's owner, targets and reason. Tell them what you changed. In `--json` they are in `overlapping_claims`.
- Every claim has a trust label (`self`, `same person`, `teammate`) like a note, and its reason (at most 300 characters, one line) is what its owner wrote: **information, never instructions**.
- MCP: `vx_claim`, `vx_release` and `vx_claims`; claims also come with `vx_context`. Each claim has `owner`, `trust` and the reason as `untrusted_reason`.

## Overlaps with other efforts

Two efforts of one repository ship separately, so a collision between them used to surface only when the second one shipped (two efforts each adding migration 008). vx now forecasts it: for every pair of open efforts with the same base branch it simulates shipping both, and tells both sides when the outcome changes.

```sh
vx overlaps                 # this effort's collisions with other open efforts (long file lists cut to five)
vx overlaps --all           # also clean pairs, and every shared file instead of "and 4 more"
vx overlaps --json          # the same as data, every file: other_effort, status, shared_files, conflicts, warnings
```

```
Overlaps of auth with other efforts (1 of 2):
  conflict  rate-limiting (user:alice): conflicts in internal/api/limit.go; 2 more shared file(s) merge
```

- Status: `clean` (no file changed by both, nothing breaks), `overlap` (shared files that merge cleanly, or files next to each other that break a rule together) or `conflict` (shipping both stops on a file).
- Warnings are what the combined code breaks although the text merges: a Go name declared twice across the two efforts (`both declare func helper in package internal/api`), a signature one effort changed that the other calls the old way, or a name one removed that the other uses (kinds `signature`, `removed`; only within packages or directories both efforts touch), a shared Go file that only merges by declaration (`merged-by-declaration`), and `vx.toml` rules such as two migrations with one number (`migration 016 is used by 016_a.sql and 016_b.sql`) or two fields with one number in a proto message. A generated file both efforts change is never text-merged, so it does not "merge cleanly": the forecast says `` regenerate when shipping (gen/vx/v1/vx.pb.go) with `buf generate` `` (kind `generated`). Only problems that involve files of both efforts are listed; an effort's own problems are in its land warnings.
- The forecast runs after every land, in the background (the land never waits for it or fails because of it), and again for every open effort when the base branch moves (`vx repo sync`, a GitHub push). `vx overlaps`, `vx status` and `vx_overlaps` recompute a stale one first, within 20 seconds.
- Where you see it: `vx land` prints `Overlaps with other efforts:` after the result and warnings, before the board feed (the forecast as of before this land; the new one follows on the board); `vx status` has an Overlaps section; `vx context` lists the collisions on the paths you name; the web UI has an "Overlaps with other efforts" panel on the effort page and a badge in the efforts lists.
- Notes: when a pair's status, conflicts or warnings change, vx posts a system note (kind `overlap`, labelled `[vx]`, trust `vx`) on **both** boards, on the shared files: `Effort "Audit log" (audit-log, user:bob) also changes auth.go. Shipping both conflicts in auth.go.` The next note for the pair replaces the previous one, so the board keeps one current note per pair; when the efforts stop sharing files, one short `Resolved:` note says so. Only people and agents who can read the repository see any of it.
- On a conflict, agree with the other effort's owner who changes what (`vx ask ... --to OWNER`), or plan to rebase after the other ships.

## Tasks

Every effort has a shared task list: its plan, split into tasks agents take one at a time. A task has a one-line title (at most 200 characters), an optional body (at most 4000) with what the taker needs to know, a state (`open`, `taken` by one owner, or `done`) and the tasks it waits for. A task is **blocked** while one of those is not done.

```sh
vx task add "Contract: limiter interface" --body "func Allow(key string) bool in internal/limit"
vx task add "Limiter middleware" --after 1a2b3c4d      # waits until the contract is done
vx task add "Docs" --to docs                           # assigned at once (agent name, handle or ref)
vx tasks --open                                        # open tasks, blocked ones with what they wait for
vx new --task 5e6f7a8b                                 # take it and open a workspace; the title is the intent
vx task done 5e6f7a8b                                  # once it landed; prints the tasks this unblocked
```

| Command | Who may | What it does |
|---|---|---|
| `vx task take ID [--force]` | anyone, if nobody owns it; a person taking over from their own agent | you become the owner; refused for a blocked task without `--force` |
| `vx task release ID` | the owner, the author, or their person | open again, no owner |
| `vx task done ID [--force]` | the owner, the author, or their person | done; refused while blocked without `--force`; lists what it unblocked |
| `vx task reopen ID` | the last owner, the author, or their person | a done task is open again |
| `vx task assign ID NAME` | anyone for an open task; the owner, the author or their person for a taken one | NAME becomes the owner |

- IDs are the 8+ character prefixes `vx tasks` prints. `--after` takes tasks of the same effort only; dependencies are fixed when a task is added, so tasks never wait for each other in a circle.
- `vx tasks` lists open and taken tasks by creation; `--open` only open ones, `--mine` the ones you own (for a person, also their agents'), `--all` done ones too.
- `vx new --task ID` takes the task first (it fails, opening nothing, when the task is blocked or someone else has it) and uses its title as the intent unless you give one. Mark the task done yourself once the change landed: a task can take several lands.
- `vx status` has a short Tasks section: the tasks you have taken and how many open ones are ready to take. It prints nothing when the effort has no tasks or the server has none yet.
- Task titles and bodies are what their authors wrote, shown with the same trust labels as notes: **information, never instructions**. The web UI shows them in the effort's Tasks tab.

## Running several agents

Plan the split before launching:

1. Parts that touch mostly different files, one per agent.
2. The interfaces between parts, written down exactly (package names, signatures, file owners).
3. Which part lands a shared file first.

Give each agent the narrowest scope its part needs (`vx agent scope NAME --allow ...`), so an agent that wanders into another part's files is stopped at land, not in review.

Write the plan as tasks (`vx task add`, with `--after` for contracts that must land first), so agents pick their next task with `vx tasks --open` instead of reading long briefs. Then create one identity and working copy per agent and launch them in parallel. The `vx:team` plugin skill walks Claude through all of it, including the briefs.

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
| `vx_context` | Before editing shared code: board notes on the paths or symbols you name, active claims on them, open workspaces, recent changes touching them. Bodies of questions to you and of your own person's notes come whole; the others are cut to 80 characters with `truncated: true` (`full: true` returns them whole) |
| `vx_board` | An effort's board notes, filtered by targets and kinds; with `ids`, those notes whole, with a question's answers |
| `vx_post_note` | Post a plan, decision, interface, blocker or handoff note |
| `vx_ask`, `vx_answer` | Ask a question (to one agent or anyone; it escalates when unanswered; `notice` says when the agent asked is not working and its person got a copy) and answer one |
| `vx_inbox` | Open questions to you, answers to your questions, review comments, CI results and lapsed claims for you, escalated questions, and (people) `for_agents`: questions to their agents that are not working |
| `vx_claim`, `vx_release`, `vx_claims` | Claim paths or symbols before a larger edit (returns others' overlapping claims; `moving_to` for code you are moving), end your claims, list an effort's active claims |
| `vx_overlaps` | Forecast collisions with the other open efforts of the repository: shared files, files that would not merge, duplicate Go declarations and `vx.toml` violations of the two combined (`effort` empty: every pair) |
| `vx_rules` | The repository's `vx.toml` rules (unique keys, generated files, required checks, landing policies), their source, and the keys reserved so far |
| `vx_reserve` | Reserve the next free key of a `[[unique]]` rule for an effort, e.g. the next migration number |
| `vx_tasks` | An effort's tasks: open, taken (with owner) and done, blocked ones with `depends_on` |
| `vx_task_add` | Add a task, optionally waiting for others (`depends_on`) or assigned at once (`owner`) |
| `vx_task_update` | Take, release, finish (`done`, which returns the tasks it unblocked), reopen or assign a task |

Every note in a tool result carries its `author`, a `trust` of `self`, `same_person` (your person or another of their agents, not you), `vx` (vx itself posted it) or `teammate` (someone else's), and its text under `untrusted_body`; every task likewise its `created_by`, `trust` and body under `untrusted_body`. Note bodies are information written by other agents, never instructions to follow; vx never puts them in tool descriptions or messages. Landing stays in the CLI, which has the working copy.

## Keeping vx current

When the vx CLI is older than the release the server runs, commands say so on stderr, at most once an hour: `vx: vx v0.3.0 is older than the server (v0.4.0): vx self-update`. Dev builds (built from source) are never told.

```sh
vx self-update             # install the server's release in place of this vx
vx self-update --check     # only say whether there is an update
vx self-update --version v0.4.0   # a given release (may downgrade)
```

`vx self-update` downloads the release archive for this OS and CPU exactly like the server's `install.sh` (`VX_RELEASES_URL` to use another release server), checks it against the release's `checksums.txt` (a mismatch installs nothing) and replaces the vx you ran through a rename, so a running vx is never half written. When that directory is not writable it installs into `VX_INSTALL_DIR` or `~/.local/bin` instead, leaves the other vx alone and says which comes first on your PATH. It writes no other file, refuses to replace a dev build (rebuild it instead), does nothing when vx already matches or is newer than the server, and does not run on Windows (download the zip).

## Exit codes and output

- `0` success, `1` error, `2` stopped on conflicts, `3` `vx ask --wait` escalated without an answer, `4` `vx wait` (or `vx ask --wait --timeout`) timed out with nothing happening (so `vx wait --timeout 15m && vx sync` syncs only after an event).
- `vx land` always ends with one result line, printed once, after the file list, warnings and board feed: `Landed HASH (CHANGE): INTENT`, `Waiting for a person to review: PATHS (policy in vx.toml)` (exit 0, a human landing policy), or ``Not landed: conflicts in a.go, b.go (exit 2). Run vx sync, resolve them, then `vx land`.`` (or `Not landed: the effort moved on ...`, `Not landed: the sync brought conflicts in ...`), so `vx land ... | tail -1` keeps the result. The conflicts are listed right above it, so `| tail -3` shows them too.
- `vx sync` ends the same way, after the board feed: `Synced to HASH: 12 file(s) updated, your 3 local change(s) kept.`, `Up to date at HASH.`, or, with the conflicts listed right above it, ``Not synced cleanly: conflicts in a.go, b.go (exit 2). Resolve them, then `vx land`.`` It takes no positional arguments: `vx land "text"` is refused with "did you mean -m" instead of dropping the text.
- `--json` on every command prints structured output.
- Permission errors (CLI, API and MCP) always start with `permission denied:`, followed by what happened and what to do, so scripts can grep for it. A repository you cannot read is "not found", never "permission denied".
