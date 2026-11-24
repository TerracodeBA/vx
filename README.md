# vx

A workbench where agents from many people build together before the code goes to git.

Teams keep GitHub, GitLab or any git host as the place code officially lives. vx sits in front of it. Agents work in a shared **effort**, each in its own **workspace**, and land small changes into the effort. The server merges concurrent work automatically and only stops on real conflicts. When the work is ready, vx ships it to the git host as clean pull requests.

Design: [vx design draft](https://claude.ai/artifact/8AJCAZHN14rYS4uRgkauBL)

## Status

Phase 1 done. Phase 2 (shared context board, leases, MCP) is next.

| Step | What | State |
|---|---|---|
| 0 | Project setup, Postgres, migrations, CI | done |
| 1 | Storage core: blobs, trees, changesets, BLAKE3, three-way merge | done |
| 2 | Server and CLI: efforts, workspaces, land with compare-and-swap, sync | done |
| 3 | Git import: follow any branch of a git or GitHub repo, chosen on the fly | done |
| 4 | Shipping: replay onto the target, clean commits, `vx/` branch, pull request | done |
| 5 | Three Claude Code agents build one feature through vx | done: [sandbox PR #1](https://github.com/TerracodeBA/vx-sandbox/pull/1) |

## Quick start

```sh
make db                                   # Postgres in Docker on 127.0.0.1:54329
make build                                # bin/vx and bin/vx-server

bin/vx-server user-add alice --name "Alice" --email alice@example.com
bin/vx-server serve &                     # listens on 127.0.0.1:7070

bin/vx login --server http://127.0.0.1:7070 --token <token from user-add>
bin/vx repo create shop-api
bin/vx effort new "Rate limiting" --repo shop-api
bin/vx clone shop-api rate-limiting
cd rate-limiting
vx new "Add limiter middleware"
# ...edit files...
vx land --check "go test ./...=pass:142"
```

Give each agent its own identity, so every change records who made it and for whom:

```sh
vx agent add backend          # prints a token for agent:alice/backend
export VX_TOKEN=<that token>  # in the agent's environment
export VX_MODEL=claude-opus-5-5
```

## Web UI

`vx-server` serves a web UI at its address, for example http://127.0.0.1:7070. Sign in with your token.

- **Efforts:** every repository and effort, with who is working right now.
- **Effort:** a live timeline of every land (agent, person, model, files, evidence, merged or not), the open workspaces, per-agent activity and shipments. It updates by itself as agents land.
- **Change:** the full diff of one change with its provenance and evidence.
- **Ship:** the commits that would go to the git host, the pull request description, and a button to push.

## Connecting GitHub

```sh
vx repo create acme/shop-api --github acme/shop-api
vx effort new "Rate limiting" --repo acme/shop-api            # starts on the default branch
vx effort new "Hotfix" --repo acme/shop-api --from release/1.4 # any branch, imported on first use
```

The server talks to GitHub in one of two ways:

- **With `VX_GITHUB_TOKEN`** set for `vx-server`: fetch and push over HTTPS, and pull requests are opened and updated through the API.
- **Without a token**: fetch and push over SSH with the server user's SSH agent and `known_hosts`, exactly like `git`. `vx ship open` pushes the branch and prints the link where you open the pull request with one click.

vx only ever pushes branches under `vx/`. It never pushes to `main` and never merges.

## Shipping

```sh
vx ship plan            # the commits this effort would ship, replayed onto the latest target
vx ship open --draft    # push vx/<effort>, open or update the pull request
vx ship list
```

Each landed change becomes one commit with its intent as the message and provenance in trailers:

```
Add rate limit middleware

Vx-Change-Id: kxqr-7m2p
Vx-Effort: rate-limiting
Vx-Agent: agent:alice/backend
Vx-On-Behalf-Of: user:alice
Vx-Model: claude-opus-5-5
Vx-Evidence: go test ./... pass (142 passed, 0 failed)
```

The commit author is the person the agent works for. If the target branch moved since the effort started, every change is merged onto its new head first, so the pull request is up to date with no extra step.

## Commands

| Command | What it does |
|---|---|
| `vx login --server URL --token T` | Save server and token |
| `vx whoami` | Show who the token belongs to |
| `vx agent add NAME` | Create an agent that acts for you |
| `vx repo create NAME [--github owner/repo \| --git URL]` | Register a repository |
| `vx repo sync [--branch B]` | Import new upstream commits |
| `vx effort new NAME [--from BRANCH]` | Start an effort on a branch |
| `vx effort list` / `vx effort show` | Inspect efforts and who works in them |
| `vx clone REPO EFFORT [DIR]` | Check out an effort |
| `vx new INTENT` | Open a workspace for one task |
| `vx status` / `vx diff` | Local changes, effort drift, others at work |
| `vx land [--check NAME=pass:N]` | Land into the effort, merging onto others' work |
| `vx sync` | Bring in others' changes, keeping your edits |
| `vx abandon` | Close a workspace without landing |
| `vx log [--files]` / `vx show REF` | History with intent, agent and evidence |
| `vx ship plan` / `vx ship open [--draft] [--to BRANCH]` | Ship the effort as a pull request |

Every command accepts `--json` for agents. Exit code 2 means a land or sync stopped on conflicts.

## How landing works

1. The CLI snapshots the working copy into trees and blobs (BLAKE3, zstd) and uploads only what the server lacks.
2. If the effort has not moved since the workspace's base, the snapshot becomes the new head.
3. If it has, the server runs a three-way merge of the snapshot onto the current head: file by file, then line by line. Only real conflicts are sent back.
4. The effort head moves with a compare-and-swap on one Postgres row. A land that loses the race merges again and retries, so concurrent lands never lose work.

## Layout

```
cmd/vx, cmd/vx-server   binaries
proto/                  API definition (buf, connect-go)
internal/hash           BLAKE3 object hashes
internal/blob           content-addressed object store
internal/object         trees, manifests, changesets
internal/merge          line diff, unified diff, three-way text merge
internal/engine         tree-level three-way merge
internal/store          Postgres metadata and migrations
internal/service        application logic (land, efforts, workspaces)
internal/api            connect-go handlers
internal/client         Go client with batched object transfer
internal/workcopy       working copy: scan, stat index, apply
internal/cli            the vx command line
```

## Development

```sh
make test        # full suite with Postgres and the race detector
make test-short  # without a database
make generate    # after editing proto/
make lint
```
