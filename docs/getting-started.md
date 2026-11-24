# Getting started

This guide sets up vx for one person on one machine: the server, the CLI, a first repository and an effort. It takes about ten minutes.

Prefer to have Claude do it? Install the plugin (`/plugin marketplace add TerracodeBA/vx`, then `/plugin install vx@vx`) and ask *"set up vx for me"*.

## 1. Run the server

You need Docker with the compose plugin.

```sh
git clone https://github.com/TerracodeBA/vx && cd vx/deploy
docker compose up -d
curl -s http://127.0.0.1:7070/healthz        # ok
```

This starts Postgres and `vx-server`, bound to 127.0.0.1:7070. Data lives in two Docker volumes.

If the image cannot be pulled, build it: `docker compose up -d --build`.

## 2. Create your user

```sh
docker compose exec vx vx-server user-add alice --name "Alice Example" --email alice@users.noreply.github.com
```

The command prints a token once. The name and email are used as the git author of shipped commits; your GitHub no-reply address works well.

## 3. Install the CLI and log in

From the [latest release](https://github.com/TerracodeBA/vx/releases), or with Go 1.26+:

```sh
go install github.com/TerracodeBA/vx/cmd/vx@latest
vx login --server http://127.0.0.1:7070 --token <your token>
vx whoami                                     # user:alice
```

## 4. Connect a repository

```sh
vx repo create acme/shop-api --github acme/shop-api   # GitHub
vx repo create shop --git /path/or/url/to/repo         # any git remote or local path
vx repo create scratch                                 # vx only, no upstream
```

vx imports the default branch right away. How it reaches GitHub depends on the server:

- **Without `VX_GITHUB_TOKEN`**: over SSH with the server user's keys. In Docker, that means the container needs SSH access; for a personal setup it is often simpler to run `vx-server` directly (see below).
- **With `VX_GITHUB_TOKEN`** (in `deploy/.env`): over HTTPS, and vx opens pull requests itself. Use a fine-grained token with Contents and Pull requests read/write.

## 5. Start an effort and work in it

```sh
vx effort new "Rate limiting" --repo acme/shop-api --goal "Limit login attempts per IP"
vx clone acme/shop-api rate-limiting ~/work/rate-limiting
cd ~/work/rate-limiting

vx new "Add limiter middleware"
# edit files
vx status
vx diff
vx land --run "go test ./..."
```

Efforts can start from any branch: `vx effort new "Hotfix" --repo acme/shop-api --from release/1.4`. Inside a git checkout, `vx effort new` uses the branch you are on.

## 6. Add agents

Each agent gets its own identity that acts for you:

```sh
vx agent add backend --env-file ~/.vx-agents/backend.env
source ~/.vx-agents/backend.env
vx clone acme/shop-api rate-limiting ~/work/backend
```

The env file holds the agent's token (mode 600). Point a coding agent at that directory and tell it to use vx; with the Claude Code plugin installed it knows the workflow. See [agents.md](agents.md).

## 7. Watch and ship

Open http://127.0.0.1:7070 and sign in with your token. The effort page updates live as agents land.

When the work is ready:

```sh
vx ship plan --repo acme/shop-api rate-limiting   # the commits and PR text
vx ship open --repo acme/shop-api rate-limiting --draft
```

## Running without Docker

Needs Go 1.26+ and Postgres 16+.

```sh
git clone https://github.com/TerracodeBA/vx && cd vx
make db                                  # Postgres in Docker on 127.0.0.1:54329 (or use your own)
make build
VX_DATA_DIR=~/.vxdata bin/vx-server serve
bin/vx-server user-add alice --name "Alice Example"
```

| Variable | Default | Meaning |
|---|---|---|
| `VX_DATABASE_URL` | `postgres://vx:vx@127.0.0.1:54329/vx?sslmode=disable` | Postgres connection |
| `VX_DATA_DIR` | `.vxdata` | Objects and git mirrors |
| `VX_ADDR` | `127.0.0.1:7070` | Listen address |
| `VX_GITHUB_TOKEN` | empty | GitHub token; without it, GitHub uses SSH |

CLI environment: `VX_SERVER`, `VX_TOKEN`, `VX_CONFIG` (config file path), `VX_REPO` (default repo), `VX_MODEL` and `VX_SESSION` (recorded on each change).
