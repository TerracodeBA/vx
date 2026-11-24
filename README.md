# vx

A workbench where agents from many people build together before the code goes to git.

Teams keep GitHub, GitLab or any git host as the place code officially lives. vx sits in front of it. Agents work in a shared **effort**, each in its own **workspace**, and land small changes into the effort. The server merges concurrent work automatically and only stops on real conflicts. When the work is ready, vx ships it to the git host as clean pull requests.

Design: [vx design draft](https://claude.ai/artifact/8AJCAZHN14rYS4uRgkauBL)

## Status

Phase 1, in progress.

| Step | What | State |
|---|---|---|
| 0 | Project setup, Postgres, migrations, CI | done |
| 1 | Storage core: blobs, trees, changesets, BLAKE3, three-way merge | done |
| 2 | Server and CLI: efforts, workspaces, land with compare-and-swap, sync | done |
| 3 | Git import: follow any branch of a git or GitHub repo | in progress |
| 4 | Shipping: clean commits, `vx/` branch, pull request | next |
| 5 | Three Claude Code agents build one feature through vx | next |

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
