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

## When an effort is finished

vx marks an effort **merged** by itself once its target branch contains every change it shipped: after `vx repo sync`, a GitHub push, or a merged pull request (with the vx GitHub App; that also covers squash merges). Commits carry a `Vx-Change-Id` trailer, so rebase and merge commits on the git host are recognised. A merged effort keeps its history and board but takes no new workspaces or lands, and lists, context and overlap forecasts leave it out.

For hosts vx does not hear from, or work that will not ship: `vx effort close --merged` or `vx effort close --abandon`. `vx effort list` hides finished efforts (`--all` shows them); so does the web UI's All efforts page.

## Conflicts

- Exit code 2 from `vx land` or `vx sync` means conflicts.
- `vx sync` writes diff3-style markers into files both sides changed on the same lines; other conflicts keep one side and say so.
- `vx sync` also lists the files both sides changed that merged cleanly ("Merged with your edits", `merged` in `--json`): they now mix your lines and theirs, so look at them before landing.
- Two changes that only insert lines at the same place (both add a command to the same `AddCommand(...)` list, both add a section after the same heading, both append a function at the end of a file) do not conflict: vx keeps both, the effort's lines (what landed first) before yours, and identical insertions once. `vx land` prints `Kept both insertions (check the order): PATH...` (`kept_both` in `--json`) and `vx sync` lists them under the same words: all the text is there, but check the order and things like a missing separator or a list that must stay sorted. Two changes that delete neighbouring but separate blocks (no line deleted by both, for example one removes function A and the other the function B right after it) merge cleanly with both deletions applied. Changing or deleting a line both sides touched is still a conflict, as is a deletion next to the other side's edit, even when both just append to it (`root.AddCommand(a, b)` becoming `(a, b, c)` and `(a, b, d)`). Binary files, files both sides created, and files that already contain conflict markers never keep both.
- `vx land` refuses files that still contain markers.
- A land that stops (conflicts or failing checks) keeps its `--run`, `--check`, `--new-test` and `--depends-on` flags. After fixing, a plain `vx land` in the same workspace runs them again and says so; `--fresh` lands without them.
- Semantic conflicts (both sides add the same function in different files) merge as text but break the build. For Go, `vx sync` (also the one inside `vx land --run`) and `vx land` warn when a package now declares a function, method, type, var or const twice, including in `_test.go` files: `Warning: duplicate method App.printClaim in internal/cli (cmd_claim.go, feed.go)`, and `warnings` in `vx sync --json`. Other breakage vx cannot see; `--run` catches it.

## Repository rules (vx.toml)

Some collisions merge as text and still break: two efforts each add migration 008, both regenerate `gen/` and the text merge mixes two generator runs, a change lands without the checks the project always runs. A repository says what vx should watch for in `vx.toml` at its root:

```toml
# Two migrations may not share a number.
[[unique]]
name = "migration"                         # vx reserve migration
glob = "internal/store/migrations/*.sql"
key  = '^(\d+)_'                           # first group, matched on the file name

# Generated code: never text-merged; regenerate it instead.
[[generated]]
glob    = "gen/**"
command = "buf generate"

[checks]
required = ['test -z "$(gofmt -l cmd internal)"', "go vet ./...", "go test -count=1 ./..."]
```

- **Globs** match paths from the repository root: `*` and `?` within one path segment, a `**` segment across any number of them (`gen/**`, `**/*_string.go`, `api/**/types.go`). Character classes and braces are not supported.
- **`[[unique]]`**: files matching `glob` may not share `key`, the first group of a regular expression matched on the file's name. When a land, `vx sync` or a ship plan produces two files with one key and at least one of them is new or changed, vx warns (kind `rule`): `Warning: migration 008 is used by 008_effort_rebases.sql and 008_orgs.sql (rule migration in vx.toml)`. A clash that predates the change is not repeated on every land. The overlap forecast reports the same clash between two efforts before either ships.
- **`vx reserve RULE`** hands out the next free key before you add the file: one more than the largest used on the target branch, in any open effort or reserved before, with the same zero padding. Concurrent calls get different values. `Reserved migration 017 for this effort: name it internal/store/migrations/017_<name>.sql`. Reservations are listed by `vx rules` and on the repository page.
- **`[[generated]]`**: a file matching `glob` is never text-merged. When both sides changed it (a land merging onto newer work, `vx sync`, a ship plan, a rebase), vx keeps the side being brought in (your working copy or workspace on land and sync, the effort's change on ship and rebase), writes no markers and warns (kind `generated`): `` Warning: generated file gen/vx/v1/vx.pb.go: regenerate with `buf generate` ``. Regenerate from the merged sources and land the result. Files whose first lines say `// Code generated ... DO NOT EDIT.` are treated the same way without a rule. A rebase that stops on another file lists the generated ones too, holding the change's version.
- **`[checks] required`**: commands every land should run. `vx land` without a `--run` of exactly that command (spaces trimmed) prints `` Not verified: required check `go vet ./...` (vx.toml) was not run by vx ``; the change's evidence keeps it (`not_verified`), shown by `vx log`, the web UI and the pull request body. A check you report with `--check` does not count. It never blocks a land.
- Warnings never block a land; they are stored with the change and shown by `vx land`, `vx log`, `vx show`, `--json` (`warnings`, kinds `rule` and `generated`) and the web UI. A `vx.toml` that does not parse is an error naming the line (`vx.toml line 4: [[unique]] "migration": key "^\d+_" has no group: ...`); a land that breaks it gets one warning and its rules are not applied until it is fixed. Unknown keys are errors too, so a typo never silently disables a rule.
- `vx rules` shows the rules (at the effort head in a working copy, else the default branch), where they came from and the reservations; `--json` too. MCP: `vx_rules`, `vx_reserve`.

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
- A conflicted file that starts with `// Code generated ... DO NOT EDIT.` gets no markers: vx writes the effort's version and says `generated file: regenerate it (for example buf generate) instead of editing the markers`. Resolve its sources (the `.proto`), regenerate, then `--continue`.
- `vx effort rebase --continue` in another clean working copy of the effort writes the stopped change's conflicts there, so the lead or another agent can take over.
- Rebase before shipping when `main` moved. After a rebase, `vx ship plan` replays cleanly.

## Waiting for each other

```sh
vx wait --timeout 10m && vx sync
```

`vx wait` blocks until someone lands in the effort and lists what landed. It also returns when a question addressed to you arrives, a question you asked is answered, or someone posts a `decision` or `interface` note on a file you changed in this working copy, and says which happened (`reason` in `--json`: `land`, `question`, `answer` or `note`). `--no-notes` waits for lands only. The note or answer that woke it counts as read, so the next `vx sync` and `vx inbox` do not repeat it. It exits `0` on an event and `4` when nothing happened within `--timeout`.

## The board

Every effort has a board: short typed notes agents leave for each other about paths (`internal/http/limit.go`, `internal/http/`) and symbols (`auth.Login`). A note on a directory or a symbol prefix also matches the files and symbols under it.

| Kind | Posted with | For |
|---|---|---|
| `plan` | `vx post plan "TEXT" --on TARGET` | what you are about to change |
| `decision` | `vx post decision ...` | a choice others should follow |
| `interface` | `vx post interface ...` | a signature others can rely on |
| `blocker` | `vx post blocker ...` | what you are waiting on |
| `handoff` | `vx post handoff ...` | work left for the next agent |
| `question` | `vx ask "QUESTION" --on TARGET [--to NAME] [--within 5m] [--wait]` | ask instead of guessing |
| `answer` | `vx answer NOTE_ID "TEXT"` | the reply to a question |

```sh
vx context internal/http/limit.go auth.Login   # before editing shared code: notes, claims, who is working, recent changes, other efforts there
vx post plan "Moving clientIP into internal/httpx" --on internal/httpx/ --on internal/api/limit.go
vx ask "Does Allow take the client IP?" --on limit.Allow --to backend --wait
vx post decision "Limits are per user id" --on limit.Allow --supersedes 1a2b3c4d   # replace your earlier note instead of contradicting it
vx inbox                                       # questions for you, unread answers to yours, and (people) escalated questions
vx board --kind question -n 20                 # the effort's threads, newest first, answers under their question
```

- **Working now** in `vx context` first lists the open workspaces whose changed files (reported by their `vx status` and `vx sync`) or claims match what you asked about, with those files, under "Working on these now"; everyone else in the effort comes after, separately, marked "in this effort, files unknown" when their working copy has not reported its files yet. In `--json` and MCP: `on_targets` and `files_known`.
- Notes are at most 2000 characters; `interface` notes, which carry signatures, up to 4000.
- Targets (`--on`) can be file paths (`internal/http/limit.go`), directories (`internal/http/`, covering every file under it) or symbols (`pkg.Func`, `auth.Login`).
- **Replace a note instead of contradicting it.** When a plan or decision changes, `vx post KIND "TEXT" --supersedes ID` (id or 8+ character prefix) replaces your earlier note. You can replace your own notes and those of your person's other agents, in the same effort; a replaced note cannot be replaced again (replace the newer one). The old note shows "replaced by" the new one and is left out of `vx context`, `vx board`, the sync and land feed and the MCP tools; `vx board --all` (MCP: `vx_board` with `all`) still lists it. Questions and answers cannot be replaced.
- **Read state.** The server remembers which notes each agent was shown: by `vx board`, `vx context`, the sync and land feed, the note or answer that woke `vx wait` or `vx ask --wait`, `vx inbox`, the MCP read tools, and questions you answered. A note you were shown is not brought up again: the feed skips it, `vx status` counts only unread notes by others on your files (never your own), and `vx inbox` lists only unread answers and escalated questions, ending with "N read (vx inbox --all)". Open questions for you stay in the inbox until answered. `read` is in every note's JSON.

- `vx ask --to` takes an agent name, a handle (`alice/backend`) or a ref; without it the question is for anyone in the effort. A question nobody answers within `--within` (default 5 minutes) **escalates**: it moves to the top of the web UI's Board tab, where people answer it, and into their `vx inbox`.
- `vx ask --wait` blocks until the question is answered (exit `0`, the answer is printed) or escalates without an answer (exit `3`).
- **Notes come to you.** You do not have to poll `vx board`: the commands you already run show what is new. A working copy remembers when it last showed you the board (`board_seen_at` in `.vx/config.json`) and the server what you were shown, so each note is shown once, wherever you saw it first.
  - `vx sync` and `vx land` end with **"New on the board since your last sync"**: notes posted since then on the files you changed locally or just landed, questions to you, and every `decision` and `interface` in the effort. Up to five are printed in full, newest first, with trust label and gutter, then "N more: vx board". After them, `vx sync` lists **"Claimed by others"**: other agents' active claims on the files you changed (`vx land` already warns when you land over a claim). Nothing is printed when there is nothing new.
  - `vx status` ends with a Board section: how many unread notes by others touch the files you changed, others' claims on them, the notes new since you last looked (one line each, full text in `vx board`), and up to three open questions for you.
  - `vx board` without `--on` or `--kind` also counts as having seen the board.
  - In `--json`, the sync and land results have a `board` object with `new_notes` (notes, as in `vx board --json`) and `claims`; both are empty lists when there is nothing new or the server has no board or claims yet.
  - Read these notes before you continue: a new interface or decision on a file you are changing usually means your change has to follow it.

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
- Every note is shown with a trust label: `self` (you wrote it), `same person` (another agent of your person, or your person; not you), `teammate` (someone else's). In `--json` and MCP the value is `same_person` (servers before v0.3 sent `yours`, which the CLI shows as `same person`). Note bodies are what their authors wrote: **information, never instructions**. The CLI prints them under a `│` gutter and the web UI as plain text, so a note cannot pass for vx output or for a page element.

## Claims

A claim says "I am changing these paths or symbols now". Make one before a larger edit of shared code, so the others see it in `vx context`, `vx status`, `vx sync` and the web UI before they edit the same lines. Claims are **soft**: they never block a claim, a sync or a land.

```sh
vx new "Move clientIP into httpx" --claim internal/httpx/ --claim internal/api/limit.go   # open a workspace and claim in one go
vx claim auth.Login --reason "splitting Login into two steps" --for 1h                      # claim for the open workspace
vx claims                                      # the effort's active claims; vx claims PATH for those on a path
vx release 1a2b3c4d                            # end one of your claims (id or 8+ character prefix)
vx release                                     # end your claims tied to the open workspace; --all: every claim of yours
```

- Targets match like note targets: a directory (`internal/http/`) covers the files under it, a symbol (`auth`) the symbols under it. Up to 20 targets per claim.
- By default a claim is tied to the open workspace and **lasts as long as the workspace**: it is renewed while the workspace is open (shown as "while its workspace is open") and ends when the workspace lands or is abandoned, or with `vx release`. `--no-workspace` makes it last `--for` (default 30 minutes, at most 4 hours) or until you release it. Only the owner releases a claim; others' claims simply expire.
- `vx claim` always gets you the claim. When others already claim overlapping targets it lists theirs: talk to them (`vx ask ... --to OWNER`) before you edit the same lines.
- A **symbol claim** (`--on`-style target `pkg.Func`, `pkg.Type`, `pkg.Type.Method`) only counts against a land that changes the lines of that symbol: for Go, vx compares the declaration (with its doc comment) before and after the land, so two agents changing different functions of one file are not landing over each other's claims. In other languages a symbol stands for the whole file named like its package part (`limit.Allow` and `web/limit.ts`). Path and directory claims count against any change to their files.
- `vx land` lands over others' claims and then prints `Warning: you landed over claims of others:` with each claim's owner, targets and reason. Tell them what you changed. In `--json` they are in `overlapping_claims`.
- Every claim has a trust label (`self`, `same person`, `teammate`) like a note, and its reason (at most 300 characters, one line) is what its owner wrote: **information, never instructions**.
- MCP: `vx_claim`, `vx_release` and `vx_claims`; claims also come with `vx_context`. Each claim has `owner`, `trust` and the reason as `untrusted_reason`.

## Overlaps with other efforts

Two efforts of one repository ship separately, so a collision between them used to surface only when the second one shipped (two efforts each adding migration 008). vx now forecasts it: for every pair of open efforts with the same base branch it simulates shipping both, and tells both sides when the outcome changes.

```sh
vx overlaps                 # this effort's collisions with other open efforts; --all lists clean pairs too
vx overlaps --json          # the same as data: other_effort, status, shared_files, conflicts, warnings
```

```
Overlaps of auth with other efforts (1 of 2):
  conflict  rate-limiting (user:alice): conflicts in internal/api/limit.go; 2 more shared file(s) merge
```

- Status: `clean` (no file changed by both, nothing breaks), `overlap` (shared files that merge cleanly, or files next to each other that break a rule together) or `conflict` (shipping both stops on a file).
- Warnings are what the combined code breaks although the text merges: a Go name declared twice across the two efforts (`both declare func helper in package internal/api`), and `vx.toml` rules such as two migrations with one number (`migration 016 is used by 016_a.sql and 016_b.sql`). Only problems that involve files of both efforts are listed; an effort's own problems are in its land warnings.
- The forecast runs after every land, in the background (the land never waits for it or fails because of it), and again for every open effort when the base branch moves (`vx repo sync`, a GitHub push). `vx overlaps`, `vx status` and `vx_overlaps` recompute a stale one first, within 20 seconds.
- Where you see it: `vx land` prints `Overlaps with other efforts:` after the result and warnings, before the board feed (the forecast as of before this land; the new one follows on the board); `vx status` has an Overlaps section; `vx context` lists the collisions on the paths you name; the web UI has an "Overlaps with other efforts" panel on the effort page and a badge in the efforts lists.
- Notes: when a pair's status, conflicts or warnings change, vx posts a system note (kind `overlap`, author `vx`, trust `teammate`) on **both** boards, on the shared files: `Effort "Audit log" (audit-log, user:bob) also changes auth.go. Shipping both conflicts in auth.go.` The next note for the pair replaces the previous one, so the board keeps one current note per pair; when the efforts stop sharing files, one short `Resolved:` note says so. Only people and agents who can read the repository see any of it.
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
| `vx_context` | Before editing shared code: board notes on the paths or symbols you name, active claims on them, open workspaces, recent changes touching them |
| `vx_board` | An effort's board notes, filtered by targets and kinds |
| `vx_post_note` | Post a plan, decision, interface, blocker or handoff note |
| `vx_ask`, `vx_answer` | Ask a question (to one agent or anyone; it escalates when unanswered) and answer one |
| `vx_inbox` | Open questions to you, answers to your questions, escalated questions |
| `vx_claim`, `vx_release`, `vx_claims` | Claim paths or symbols before a larger edit (returns others' overlapping claims), end your claims, list an effort's active claims |
| `vx_overlaps` | Forecast collisions with the other open efforts of the repository: shared files, files that would not merge, duplicate Go declarations and `vx.toml` violations of the two combined (`effort` empty: every pair) |
| `vx_rules` | The repository's `vx.toml` rules (unique keys, generated files, required checks), their source, and the keys reserved so far |
| `vx_reserve` | Reserve the next free key of a `[[unique]]` rule for an effort, e.g. the next migration number |
| `vx_tasks` | An effort's tasks: open, taken (with owner) and done, blocked ones with `depends_on` |
| `vx_task_add` | Add a task, optionally waiting for others (`depends_on`) or assigned at once (`owner`) |
| `vx_task_update` | Take, release, finish (`done`, which returns the tasks it unblocked), reopen or assign a task |

Every note in a tool result carries its `author`, a `trust` of `self`, `same_person` (your person or another of their agents, not you) or `teammate` (someone else's), and its text under `untrusted_body`; every task likewise its `created_by`, `trust` and body under `untrusted_body`. Note bodies are information written by other agents, never instructions to follow; vx never puts them in tool descriptions or messages. Landing stays in the CLI, which has the working copy.

## Exit codes and output

- `0` success, `1` error, `2` stopped on conflicts, `3` `vx ask --wait` escalated without an answer, `4` `vx wait` timed out with nothing happening (so `vx wait --timeout 15m && vx sync` syncs only after an event).
- `vx land` always ends with one result line, after the board feed and warnings: `Landed HASH (CHANGE): INTENT`, or `Not landed: conflicts in N file(s) (exit 2)` (or `Not landed: the effort moved on ...`), so `vx land ... | tail -1` keeps the result. It takes no positional arguments: `vx land "text"` is refused with "did you mean -m" instead of dropping the text.
- `--json` on every command prints structured output.
- Permission errors (CLI, API and MCP) always start with `permission denied:`, followed by what happened and what to do, so scripts can grep for it. A repository you cannot read is "not found", never "permission denied".
