# Checks

Companies have their own rules for what must be proven before work lands or ships: unit tests, a security review, a browser test of the pull request, recorded test output, a person's sign-off. vx does not invent those rules. A repository (or its organization) defines them as **checks**; agents, CI and people run them and **report results**; vx stores the results as evidence bound to the exact files they describe and refuses a land or a ship until every blocking check has a passing result from a reporter the check allows.

vx understands the structure of a check (when it runs, which paths it covers, who runs it, whose result counts, what it produces, what it blocks), never its meaning.

- This page: defining checks, results and gates.
- [runners.md](runners.md): the work queue, leases, runners, CI tokens and a browser runner.
- [agents.md](agents.md#evidence): `vx land --run` and the evidence of a change.
- [organizations.md](organizations.md#roles-and-what-they-allow): who may report and read results.

## Concepts

- **Check**: a `[[check]]` table in `vx.toml` (versioned with the code, changed through vx like any file) or an organization rule (org admins only; a repository cannot switch it off). The older `[checks] required` and `[[policy]]` tables are checks too (see [Compatibility](#compatibility)).
- **Run check**: a check with `run = "CMD"`. The landing agent runs the command (`vx land --run CMD` or `vx check run NAME`) and vx records the result and output. Only a result vx ran counts for it.
- **Reported check**: a check with a `runner` (`agent:KIND`, `ci` or `human`). Whoever runs it reports the result with `vx check report`.
- **Result** (evidence): status (`pass`, `fail`, `neutral`, `pending`), summary, log, artifacts (screenshots, recordings, reports), structured data, who reported it (person, agent, model, CI token), how (the command of a run check) and when. A result is bound to a change and the **exact tree** it was gathered on. A new tree needs new evidence.
- **Gate**: a check with `blocks = "land"` or `blocks = "ship"` stops that step while it has no passing result on the tree being landed or shipped.

## vx.toml reference

```toml
[[check]]
name     = "unit"
run      = "go test -count=1 ./..."   # run by the landing agent; vx records the output
paths    = ["**/*.go"]                # only for changes to these files
blocks   = "land"

[[check]]
name     = "security-review"
runner   = "agent:security"           # who picks it up: an agent kind, "ci" or "human"
reporter = "independent"              # whose result counts
paths    = ["internal/api/**", "internal/auth/**"]
outputs  = { required = ["findings.json"], artifacts = ["*.md"] }
blocks   = "land"

[[check]]
name     = "browser-e2e"
when     = "ship"
paths    = ["web/**"]
runner   = "agent:browser"
reporter = "independent"
inputs   = ["diff", "changed_files", "preview_url"]
outputs  = { required = ["steps.json"], artifacts = ["*.gif", "*.png"] }
after    = ["unit"]
timeout  = "30m"

[[check]]
name     = "release-signoff"
runner   = "human"
when     = "manual"
blocks   = "ship"
```

| Key | Values | Default | Meaning |
|---|---|---|---|
| `name` | letters, digits, `-`, `_`, `.` | required | Unique among the checks; `vx check report NAME` uses it. |
| `when` | `land`, `ship`, `manual` | `land` | When the check is expected to run. `manual` checks run when someone asks (`vx check request`). |
| `paths` | globs | every change | The check applies only to changes that touch one of these paths. Globs as elsewhere in vx.toml: `*` and `?` within a segment, `**` across segments. |
| `run` | a shell command | none | A run check: the landing agent runs it. |
| `runner` | `agent:KIND`, `ci`, `human` | none | Who picks up a reported check ([runners.md](runners.md)). A check needs `run` or `runner`. |
| `reporter` | `self`, `independent`, `ci`, `human` | `ci` for runner `ci`, `human` for runner `human`, else `self` | Whose result counts (see [Reporters](#reporters)). |
| `allow_same_person` | `true`, `false` | `false` | With `reporter = "independent"`: another agent of the author's person counts too (the company allows it). |
| `inputs` | names | none | What the runner gets: `diff`, `changed_files`, `preview_url`, ...; `CHECK.FIELD` and `CHECK.artifact:NAME` are an earlier check's outputs (pipelines). |
| `outputs.required` | names | none | What a passing result must include: an artifact name, the data file's name, or a top-level key of the data. |
| `outputs.artifacts` | globs on file names | any name | The names artifacts may have. |
| `after` | check names | none | Pipelines: the check runs once these passed on the same tree. A cycle is refused. `after` may name an organization check. |
| `blocks` | `land`, `ship`, `nothing` | `ship` for `when = "ship"`, `nothing` for `manual`, else `land` | What the check stops while it has no passing result. A check that runs at ship cannot block land. |
| `timeout` | a duration (`30m`, `1h30m`) | none | How long a runner's lease lasts ([runners.md](runners.md)). |

Mistakes are errors that name the line, like the rest of vx.toml: `vx.toml line 3: [[check]] "unit": when "merge" is not one of "land", "ship" or "manual"`. A key from a newer vx switches off only the check it is in, with a notice (`vx rules` lists it under "Ignored"); the other checks still apply. `vx rules` lists the checks.

### Reporters

| `reporter` | Whose result counts |
|---|---|
| `self` | anyone with write on the repository, the change's author included |
| `independent` | not the change's author, and not another agent of the same person (unless `allow_same_person = true`); people of other teams, their agents and CI count |
| `ci` | a CI token of the repository ([runners.md](runners.md#ci-tokens)) |
| `human` | a person, never an agent or a CI token, and not the person who made the change by hand |

A result from someone the check does not allow is stored and shown, but does not count, with the reason: `agent:alice/claude made this change; an independent reporter must (reporter = "independent") ...`. A run check counts only results that vx ran with exactly its command (`vx land --run` or `vx check run`); a result reported by hand says `reported, not run by vx`.

## Results and status

```sh
vx check status [--change ID]               # what this change still needs, per check
vx check run [NAME...]                      # run run checks here and report them, with the output as log
vx check report NAME --status pass|fail|neutral --summary TEXT \
    [--log FILE] [--artifact FILE ...] [--data FILE] [--change ID] [--tree HASH]
vx check show ID [--download DIR]           # one result; --download saves its log, artifacts and data
```

**Which change and tree.** Without `--change`, a check command is about your open workspace's change with the files as they are now (vx uploads them first), so a result you report before landing is the evidence the land needs. Without a workspace it is about the effort head; `vx check report` and `vx check run` then refuse local changes no workspace holds and a working copy behind the head. With `--change ID` it is about that change's newest tree on the effort; `--tree` names the exact tree of a change that has not landed (a runner gets it with its job).

```
$ vx check status
Checks of your workspace's change kxqr-7m2p (your files now, tree 3f2a1c9d0e7b):
  unit             passed   pass by agent:alice/claude at 14:02 (vx ran `go test -count=1 ./...`): passed in 41.3s [1a2b3c4d]  [blocks land]
  security-review  missing  runner agent:security: vx check report security-review --status pass --summary TEXT (by someone independent: ...)  [blocks land]
                            not counted: pass by agent:alice/claude at 14:05: no findings [5e6f7a8b]: agent:alice/claude made this change; ...
  Not needed (the change touches none of their paths): browser-e2e
Still needed to land: security-review.
```

States: `missing` (no result that counts), `pending`, `passed`, `failed`, `neutral`, and `not needed` for a check whose paths the change does not touch. The newest result that counts decides: a `fail` after a `pass` on the same tree means the check failed.

**Reporting.** `vx check report` says right away whether the result counts: `It counts: security-review is passed on this tree.` or `It does not count: ...`. A passing result must include what `outputs.required` names; artifact names must match `outputs.artifacts`; `--data` must be JSON. MCP: `vx_check_status`, `vx_check_report` (artifacts as base64) and `vx_check_show` (one artifact's bytes, or the log); the API is `CheckService` (`ReportCheck`, `CheckStatus`, `GetCheckResult`, `GetCheckArtifact`).

**Logs and artifacts** are objects of the repository: everyone who can read it can see them, nobody else can (a result in a repository you cannot read is "not found"), they count toward the organization's stored data, and the purge of a deleted organization deletes them. One artifact (or log, or data file) may be at most 20 MiB and one report 48 MiB, unless the server sets `VX_CHECK_ARTIFACT_MAX` and `VX_CHECK_REPORT_MAX` ([deploy.md](deploy.md)); a report also counts against the organization's push quota. Every result is in the operation log (`check-report`).

## Gates

**Land.** A land is refused while a check that blocks land and applies to the files the change touches has no passing result on exactly the snapshot being landed:

```
not landed: 2 checks that block land have no passing result on this change's tree 3f2a1c9d0e7b (change kxqr-7m2p):
  unit             missing  vx land --run 'go test -count=1 ./...' (or vx check run unit)
  security-review  failed   agent:bob/sec at 2026-11-26 14:02: "SQL built from input in x.go"; then runner agent:security: vx check report security-review ...
Provide them, then land again; vx check status shows what the change still needs.
Queued security-review for runner agent:security as job 7c1d2e3f (vx check jobs).
```

- A run check is satisfied by a passing `vx land --run` of exactly its command in this land, or by `vx check run` on the same files.
- Checks with a runner become jobs on the snapshot when the gate refuses ([runners.md](runners.md)); land again once their results are in.
- The checks of the effort head's `vx.toml` and of your snapshot's both apply: a change can add checks for itself, never drop one the effort has (deleting a check in the same change does not skip it).
- A gated land lands exactly the snapshot the results describe: when the effort moved meanwhile, the land is refused as moved instead of merged, and `vx land --run` syncs, runs the checks again and retries. Results on the old tree do not carry over; report again on the new one.

**Ship.** `vx ship open` (and every part of `vx ship open --split`) is refused while a check that blocks ship, whose paths the shipment touches, has no passing result on the effort head's tree. Report it on the head (`vx check report NAME` in a working copy at the head, or `vx check report NAME --change HEAD-CHANGE`); a land after that moves the head, so the check needs a new result.

## Compatibility

Existing `vx.toml` files keep working exactly as before:

- **`vx land --run CMD`** records each result as evidence of the change on the tree it ran on: under the name of the `[[check]]` whose `run` is that command, or without a name (how: the command). The changeset's evidence is unchanged: `ran_by_vx`, reused results and "gathered before the merge" show the same in `vx show`, `vx log`, the web UI, the commit trailers and the pull request.
- **`[checks] required`** commands are run checks that block nothing: a land without them is marked not verified (`not_verified`), as before. `vx check status` lists them.
- **`[[policy]]` gated** requires its commands as run checks that block land for the policy's paths, with the same refusal and `vx land --run ...` hint as before. **Human** policies also add a check `review`: a person reports it by approving the change in the existing review flow (`vx review approve`, or the web UI), and the approval is stored as that check's passing result. It cannot be reported with `vx check report`.

## Examples

A security review by another team's agent before anything under `internal/auth` lands:

```toml
[[check]]
name     = "security-review"
runner   = "agent:security"
reporter = "independent"
paths    = ["internal/auth/**"]
outputs  = { required = ["findings.json"] }
```

The reviewer's agent takes the job (`vx check next --runner security`), reads the diff and reports `vx check report security-review --change ID --tree TREE --status pass --summary "No findings" --data findings.json`; the author lands again.

A browser test of the pull request before it ships, with a recording:

```toml
[[check]]
name     = "browser-e2e"
when     = "ship"
paths    = ["web/**"]
runner   = "agent:browser"
outputs  = { required = ["steps.json"], artifacts = ["*.gif", "*.png"] }
```

`vx check report browser-e2e --status pass --summary "Checkout works" --artifact flow.gif --data steps.json` on the effort head, then `vx ship open`. [runners.md](runners.md) describes a browser runner with Claude in Chrome.

Unit tests every Go change must pass, recorded by vx:

```toml
[[check]]
name   = "unit"
run    = "go test -count=1 ./..."
paths  = ["**/*.go"]
```

`vx land --run "go test -count=1 ./..."` satisfies it in the land itself.
