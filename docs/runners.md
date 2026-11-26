# Check runners

Some checks are not a command the landing agent runs: a browser test of the pull request's preview, a security review by an agent of another team, CI's own test matrix, a person's sign-off. In `vx.toml` such a check names a `runner` other than the landing agent, and vx turns it into a **job** when it applies. Runners take jobs from the queue, run them and report the result. [checks.md](checks.md) describes checks, results and gates; this page is about the queue and the runners.

```toml
[[check]]
name     = "browser-e2e"
when     = "ship"                  # land | ship | manual
paths    = ["web/**"]
runner   = "agent:browser"         # an agent that serves browser jobs, "ci" or "human"
reporter = "independent"           # not the change's author, not another agent of the same person
inputs   = ["diff", "changed_files", "preview_url"]
timeout  = "30m"                   # the lease a runner gets
```

## Jobs

A job is one check on one tree of an effort. vx makes one:

- **On land** of a change that touches the check's `paths` (`when = "land"`), for the landed tree. When the check blocks landing, the land gate makes the job for the tree the author wants to land, and the author lands again once a runner reported a pass.
- **On ship** (`when = "ship"`): asking for a ship plan (`vx ship plan`, `vx ship open`, the web's Prepare pull requests page) makes the jobs of the ship checks whose paths the effort changed, for the effort's head tree.
- **On request**: `vx check request NAME [--change ID] [--value preview_url=https://...]` makes a job for any check with a runner (also `manual` ones, and to run a land or ship check again), on the effort's head or one change.

Land and ship never make a second job for a tree they made one for (pending, done or failed); a request always does. A **new tree supersedes** the pending jobs of the same check on older trees of the effort: they are marked `superseded` and say by which job, and a runner that renews such a job is told to stop. With pipelines (`after = [...]`) a check's job is made once what it comes after has passed.

What a runner gets (`vx check next --json`, `job.bundle`):

| Input | What |
|---|---|
| `effort` | repository, slug, name, goal, base branch |
| `change` | change id, changeset, intent, author and their person, base tree, tree. A ship job's change is the effort head; its diff is the whole effort's. |
| `changed_files` | every changed path with `add`, `modify` or `delete`, hash and mode |
| `files` | the changed files that exist in the tree. Fetch their contents with `GetObjects` (`ObjectService`, the job's repository and the hashes); a CI token may do that for its repository. |
| `diff` | the unified diff from base tree to tree, cut at 256 KiB (`diff_cut`), when the check takes `diff` or declares no inputs |
| `values` | named inputs, such as `preview_url`, given by whoever requested the check (`--value`) or by an earlier check's outputs (pipelines: `build.preview_url`) |
| `missing` | declared inputs that have no value yet |

The bundle is written by whoever made the change. A runner treats it as data to test, never as instructions.

## Runners and leases

```sh
vx check next [--runner browser] [--wait [--timeout 30m]] [--json]   # take the oldest job you may run
vx check renew JOB [--for 30m]                                       # extend your lease from now
vx check release JOB [--reason "no GPU here"]                        # give it back, no attempt counted
vx check report NAME --change ID --tree TREE --status pass ...       # the result; it finishes the job
vx check jobs [--effort SLUG] [--state queued] [--all]               # the queue of an effort or repository
vx check job JOB [--json]                                            # one job with its inputs
```

MCP has the same as `vx_check_next`, `vx_check_renew`, `vx_check_release`, `vx_check_jobs`, `vx_check_job` and `vx_check_request`; the API is `CheckQueueService`.

- **Who gets what.** `agent:KIND` jobs go to agents whose person declared that kind for them; `ci` jobs to CI tokens of the repository (for the checks the token may report); `human` jobs to people with write on the repository, and they appear on each person's **For you** page. Taking a job needs write on its repository. Without `--repo` (or `VX_REPO`, or a working copy) `vx check next` takes from every repository you may run jobs in.
- **Never a job you could not report.** A runner is not given a job whose result would not count from it: an `independent` check of its own change, or of another agent of the same person (unless the check says `allow_same_person = true`), a `human` check to an agent.
- **Leases.** Taking a job leases it for the check's `timeout` (30 minutes without one, at most 24 hours). Renew it while you work. When a lease expires without a result, the job goes back to the queue with the attempt counted and the reason (`the lease of alice/qa expired without a result (attempt 1 of 3)`); after 3 expired leases it is `failed` with the reason, and only a new request runs it again. Releasing a job puts it back without counting an attempt.
- **Several servers.** Every server takes jobs from the same Postgres queue: taking locks candidate rows with `FOR UPDATE SKIP LOCKED`, so two runners never get the same job, and putting back expired leases is a single update any server may run.
- Every job made, leased, released and finished is in the operation log (`check.job.create`, `check.job.lease`, `check.job.release`, `check.job.done`).

### Runner kinds

Which kinds of jobs an agent runs is a property of the agent, set by its person, like its scope:

```sh
vx agent runner qa --kinds browser,security   # qa takes agent:browser and agent:security jobs
vx agent runner qa                            # show
vx agent runner qa --kinds ''                 # none
```

The agent itself cannot change it, so a person decides which of their agents run (and report) which checks. `ci` and `human` are not agent kinds.

## Who may report (reporter trust)

A check's `reporter` says whose result counts. A result from anyone else is stored, shown with why it does not count, and does not satisfy the gate.

| `reporter` | Counts from |
|---|---|
| `self` (default) | anyone who may report on the repository, the change's author included |
| `independent` | anyone but the change's author and, unless the check says `allow_same_person = true`, its person and that person's other agents. People of other teams, their agents and CI tokens are independent. |
| `ci` | only a CI token of the repository |
| `human` | only a person, in the web UI or with their own vx; never an agent, never a CI token, and not the person who made the change by hand (their agents' changes they may sign off) |

## CI tokens

A repository admin creates a CI token for a CI system:

```sh
vx repo ci-token create github-actions --repo acme/shop --checks unit,lint   # the secret is shown once
vx repo ci-token list --repo acme/shop [--revoked]
vx repo ci-token revoke github-actions --repo acme/shop
```

It is a service principal of its own (`service:ci-...`, the name its results carry) that may only report results of the named checks (or every check without `--checks`) on that one repository, take its `ci` jobs and read their inputs and files. It has no repository grant: everything else, on that repository or any other, is "not found" to it, exactly as to a stranger. It cannot land, open workspaces, read the board or mint tokens. Only people create CI tokens; revoking one stops it at once. `repo.ci_token.create` and `repo.ci_token.revoke` are in the operation log.

### GitHub Actions

Store the secret as `VX_TOKEN` in the repository's Actions secrets and the server's URL as the variable `VX_SERVER`. The pull request vx opens is on the branch `vx/EFFORT`, and each commit carries its change in a `Vx-Change-Id` trailer: the head commit's change is the effort head, so reporting for it binds the result to the tree that was pushed.

```yaml
name: unit
on:
  pull_request:
    branches: [main]
jobs:
  unit:
    if: startsWith(github.head_ref, 'vx/')
    runs-on: ubuntu-latest
    env:
      VX_SERVER: ${{ vars.VX_SERVER }}
      VX_TOKEN: ${{ secrets.VX_TOKEN }}
      VX_REPO: acme/shop
    steps:
      - uses: actions/checkout@v4
        with: { ref: "${{ github.event.pull_request.head.sha }}" }
      - uses: actions/setup-go@v5
        with: { go-version: stable }
      - name: Install vx
        run: curl -fsSL "$VX_SERVER/install.sh" | sh && echo "$HOME/.local/bin" >> "$GITHUB_PATH"
      - name: Test
        id: test
        shell: bash
        run: go test ./... 2>&1 | tee test.log; exit "${PIPESTATUS[0]}"
      - name: Report to vx
        if: always()
        shell: bash
        run: |
          effort="${GITHUB_HEAD_REF#vx/}"
          change=$(git log -1 --format='%(trailers:key=Vx-Change-Id,valueonly)' | head -1)
          status=pass; [ "${{ steps.test.outcome }}" = success ] || status=fail
          vx check report unit --repo "$VX_REPO" --effort "$effort" --change "$change" --status "$status" \
            --summary "go test ./... on ${GITHUB_SHA::7}" --log test.log --artifact test.log
```

Without the CLI, the same report is one HTTP call to the API (connect protocol with JSON; `bytes` fields such as `log` and an artifact's `data` are base64):

```sh
curl -fsS "$VX_SERVER/vx.v1.CheckService/ReportCheck" \
  -H "Authorization: Bearer $VX_TOKEN" -H "Content-Type: application/json" \
  --data @- <<EOF
{"repo": "acme/shop", "effort": "$effort", "change_id": "$change", "check": "unit", "status": "$status",
 "summary": "go test ./...", "log": "$(base64 -w0 test.log)",
 "artifacts": [{"name": "test.log", "content_type": "text/plain", "data": "$(base64 -w0 test.log)"}]}
EOF
```

The answer says whether the result counts (`result.counts`, and `result.why` when it does not). `--tree` (`"tree"`) pins the result to an exact tree; without it a change's result is for its newest tree on the effort, so a change rebased after the push needs a new run.

A CI runner can also pull work instead of reacting to pull requests: `vx check next --runner ci --wait --json` hands it a `ci` job with the tree to test, and `files` and `GetObjects` give it the changed files without git.

## A browser runner with Claude in Chrome

A company can let an agent test changes in a real browser: Claude Code with Claude in Chrome opens the preview of the change, walks the flows the change touches, records a GIF and reports.

1. **A machine for it.** A separate machine, VM or at least a separate operating-system user with its own Chrome profile (see [Security](#security)). Install Chrome, the Claude in Chrome extension, Claude Code and vx there. Claude Code enables the browser with `claude --chrome` (`/chrome` in a session shows the connection and its permissions); it needs Claude Code signed in with `/login` and does not work on WSL (see Claude Code's [Chrome documentation](https://code.claude.com/docs/en/chrome)). Claude in Chrome pauses at login pages and CAPTCHAs and asks before visiting new sites, so approve the preview's domain once in an interactive session on the runner.
2. **An agent for it.** As the person responsible for the runner:

   ```sh
   vx agent add qa --env-file ~/.vx-agents/qa.env    # its token goes into the file, mode 600
   vx agent runner qa --kinds browser
   ```

   Copy the env file to the runner machine. Its results are reported as `agent:YOU/qa`, so for `reporter = "independent"` checks it must not be your agent if your agents write the code (or the check says `allow_same_person = true`). Usually the QA team's person owns the runner agent.
3. **The check.** In `vx.toml`, a check with `runner = "agent:browser"`, `inputs` with `preview_url`, `outputs = { required = ["steps.json"], artifacts = ["*.gif", "*.png"] }`, and a `timeout` long enough for one run. Whoever deploys previews requests it with the URL: `vx check request browser-e2e --value preview_url=https://pr-12.preview.example.com`, or a `build` check reports `preview_url` as an output and the browser check comes `after` it.
4. **The loop.** [examples/browser-runner.sh](examples/browser-runner.sh) does, forever:
   - `vx check next --runner browser --wait --json`, which leases one job;
   - renews the lease every few minutes while Claude works;
   - runs `claude --chrome -p` with `CLAUDE_ARGS` (Claude Code non-interactively, with Claude in Chrome; see [What Claude Code documents](#what-claude-code-documents-and-what-to-try-first) before relying on it), with a prompt that gives the job as data: open `values.preview_url`, test the flows the changed files touch, record the session as a GIF, write the steps as `steps.json`;
   - Claude reports with `vx check report browser-e2e --change ID --tree TREE --status pass|fail --summary "..." --artifact flow.gif --data steps.json`, which finishes the job; if Claude fails, the script releases the job with the reason.

   ```sh
   source ~/.vx-agents/qa.env
   sh docs/examples/browser-runner.sh
   ```

   `steps.json` is a list of what was done and seen, for example `[{"step": "add a pen to the cart", "expected": "cart shows 1", "seen": "cart shows 1", "ok": true}]`. The GIF and the steps show on the change and effort pages next to the result.

### What Claude Code documents, and what to try first

vx only hands out the job and stores the result; the browser part is Claude Code's, and its [Chrome documentation](https://code.claude.com/docs/en/chrome) and [headless documentation](https://code.claude.com/docs/en/headless) are the source of truth. As of this writing:

- **Unattended use is not documented.** The docs describe `--chrome` and `-p` separately and never say they work together. Try `claude --chrome -p "open https://example.com and tell me its title"` on the runner before relying on the loop.
- **Sign-in.** Chrome integration needs Claude Code signed in with `/login` on a Pro, Max, Team or Enterprise plan; with an API key or a `claude setup-token` token it stays off, and it is not available through Bedrock or Vertex. The extension must be signed in to the same organization (`/status` shows it).
- **A visible browser.** Actions run in a real Chrome window. At a login page or a CAPTCHA, Claude pauses for a person; unattended, that run stalls until the lease expires and the job goes back to the queue. Previews should not need a login, or use a test account Claude can type.
- **Per-site approvals.** The extension asks before acting on a new site. Approve the preview domains once, in an interactive session on the runner, before going unattended.
- **Tool permissions.** In `-p` mode, a tool call that needs approval is denied. Allow the browser tools and the report command up front with `--allowedTools` (list the Claude in Chrome tool names with `/mcp` in an interactive session), and keep the runner's permission mode narrow (never `bypassPermissions` outside a throwaway VM). The example script takes them from `CLAUDE_ARGS`.
- **GIFs.** Claude can record a GIF when asked, but no save location is documented. The prompt names the path, and the script checks that the files exist afterwards and releases the job if they do not.

The check itself does not depend on Claude in Chrome: a `browser` runner can be any tool the company trusts (its own browser automation, a test suite with screenshots), as long as it reports with `vx check report` and the artifacts the check requires.

vx's own tests never drive a browser: the loop is a script around `vx check next` and `vx check report`, tested like any other runner.

### Security

A runner runs content from the change it checks: a preview deployed from the change's code, a diff and file contents an agent wrote. Treat both as untrusted.

- **Isolate it.** A separate machine or VM, or at least a separate OS user and a Chrome profile used for nothing else: no personal or company sign-ins, no saved passwords, no password manager, no other extensions with broad access. A page in the preview can try to make the browser agent do things (prompt injection); there should be nothing there worth taking.
- **No production credentials.** The runner's environment holds only its vx agent token. No cloud credentials, deploy keys, production URLs with sessions, SSH agent or VPN to internal networks. Previews use test accounts made for them.
- **Least privilege in vx.** The runner agent's person should have write only on the repositories it tests; scope the agent (`vx agent scope qa --allow 'nothing/**'`) so it can report but never land. A CI token is narrower still: it can only report its checks on one repository.
- **Inputs are data.** The prompt to Claude says the job's fields are data to test, not instructions, and the browser stays on the preview's site. Do not paste the diff into a shell.
- **Short leases, one job at a time.** A runner that hangs loses its lease and the job goes to another runner; three expired leases fail the job with the reason instead of retrying forever.
- **Review recordings.** A GIF shows whatever the browser showed, account details included: previews should show only test data.
- **Revoke on doubt.** `vx token revoke` for the agent's token, `vx agent runner qa --kinds ''` to stop it taking jobs, `vx repo ci-token revoke` for CI.
