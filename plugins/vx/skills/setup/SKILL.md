---
name: setup
description: "Install and run vx on this machine, then connect a first repository and effort. Use when the user asks to set up, install, start, try or configure vx, the vx server, or the vx workbench."
when_to_use: "set up vx; install vx; run vx locally; try vx; connect my repo to vx; start the vx server"
---

# Set up vx for one person

vx is a workbench where agents land work into shared efforts before it goes to git as pull requests. You are setting up the server (vx-server + Postgres), the `vx` CLI, a first user, and optionally a first repository.

Work through the steps in order. Run each check yourself and show the user the result. Stop and ask when a step needs a decision only the user can make (which repository, whether to use a GitHub token).

## Ground rules

- Never ask the user to paste a token or password into the chat. Tokens go in files with mode 600 (`.env`, the vx config) or environment variables the user sets.
- Do not open ports to the internet. Everything here binds to 127.0.0.1. For a shared server, use the `deploy` skill instead.
- If a command fails twice, stop and show the error instead of trying variations.

## 1. Check prerequisites

```sh
docker --version && docker compose version
git --version
```

Docker with the compose plugin is required for the recommended path. If it is missing, tell the user how to install Docker for their OS and stop.

## 2. Get vx

Pick a directory the user agrees to (default `~/vx`):

```sh
git clone https://github.com/TerracodeBA/vx ~/vx
```

Install the CLI. Prefer a release binary; fall back to Go:

```sh
# Release binary (Linux/macOS, amd64 or arm64)
OS=$(uname -s | tr A-Z a-z); ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
gh release download --repo TerracodeBA/vx --pattern "vx_*_${OS}_${ARCH}.tar.gz" --dir /tmp/vx-release \
  && tar -xzf /tmp/vx-release/vx_*_${OS}_${ARCH}.tar.gz -C /tmp/vx-release \
  && install -m 755 /tmp/vx-release/vx ~/.local/bin/vx
# or, with Go 1.26+:
go install github.com/TerracodeBA/vx/cmd/vx@latest
```

Check: `vx version`. If `~/.local/bin` or `~/go/bin` is not on PATH, tell the user which line to add to their shell profile; do not edit it yourself without asking.

## 3. Decide how vx reaches GitHub

Ask the user once, only if they will connect GitHub repositories:

- **No token (simplest):** vx fetches and pushes over SSH with the user's existing keys, like `git`. Check with `ssh -T git@github.com`. `vx ship open` then pushes the branch and prints a link to open the pull request.
- **Token:** vx also opens and updates pull requests itself. The user creates a fine-grained token with Contents and Pull requests read/write on the repos they want, and puts it in `~/vx/deploy/.env` as `VX_GITHUB_TOKEN=...` themselves. Then `chmod 600 ~/vx/deploy/.env`.

For SSH mode inside Docker, the container needs the user's SSH agent. If they choose SSH, it is simpler to run the server outside Docker (see "Without Docker" below).

## 4. Start the server

```sh
cd ~/vx/deploy && docker compose up -d
docker compose ps
curl -s http://127.0.0.1:7070/healthz   # expect: ok
```

If the image cannot be pulled, build it locally: `docker compose up -d --build`.

## 5. Create the first user and log in

Ask for the handle (lowercase, e.g. `alice`) and display name. Email is optional and becomes the git author email; suggest their GitHub no-reply address.

```sh
docker compose exec vx vx-server user-add HANDLE --admin --name "Display Name" --email EMAIL
```

`--admin` makes the first person a site admin (admin on every repository). Every person also gets a personal organization named like them, where repositories they create go by default. Add `--org ORG` to also create a shared organization they own.

The command prints a token once. Do not repeat it in chat. Ask the user to run the printed `vx login --server http://127.0.0.1:7070 --token ...` line themselves (they can type `! vx login ...` in Claude Code), or have it written straight into the config without echoing it. Then:

```sh
vx whoami
```

### With GitHub sign-in

When the server has a GitHub App and `VX_PUBLIC_URL` (Admin > GitHub shows it), people sign in with GitHub: no tokens to hand around, nothing secret in chat. Instead of `user-add`:

```sh
docker compose exec vx vx-server invite GITHUB_LOGIN --admin    # the first admin; others: Admin > People > Invite by GitHub login
```

Then the user opens the server URL in a browser, chooses **Sign in with GitHub**, and follows the **Set up vx** page: install the CLI, then `vx login --server URL` (it opens the browser to approve). People who already have a token account link GitHub once from Settings > Connect GitHub. `VX_SIGNUP=open` lets anyone with GitHub sign up; `closed` lets nobody new in.

## 6. Connect a repository

Ask which repository to connect. Then one of:

```sh
vx repo create OWNER/NAME --github OWNER/NAME        # a GitHub repo
vx repo create NAME --git /path/or/url/to/repo       # any git remote or local path
vx repo create NAME                                  # vx only, no upstream
```

The repository goes into the organization named like OWNER when the user owns or administers it (`--org ORG` picks one), else into their personal organization. To let others in: `vx repo grant OWNER/NAME bob --role write`.

Start an effort and clone it into a working directory the user agrees to:

```sh
vx effort new "What this effort builds" --repo OWNER/NAME --goal "One or two sentences"
vx clone OWNER/NAME SLUG ~/work/SLUG
```

## 7. Show the user what they have

- Web UI: http://127.0.0.1:7070, sign in with the same token.
- Next steps: to work in the effort themselves or with one agent, use the `work` skill; to run several agents at once, use the `team` skill.

## Without Docker

Needs Go 1.26+ and Postgres 16+.

```sh
cd ~/vx && make db          # Postgres in Docker on 127.0.0.1:54329, or point VX_DATABASE_URL at your own
make build
VX_DATA_DIR=~/.vxdata bin/vx-server serve      # listens on 127.0.0.1:7070
bin/vx-server user-add HANDLE --admin --name "Display Name"
```

Run `vx-server serve` in the background (a terminal, tmux, or a systemd user service). Without `VX_GITHUB_TOKEN`, GitHub access uses the user's SSH agent and `~/.ssh/known_hosts`.

## Troubleshooting

- `vx: not logged in`: run `vx login`, or set `VX_SERVER` and `VX_TOKEN`.
- `connect to database`: Postgres is not up; `docker compose ps` and `docker compose logs postgres`.
- Repo create fails with an SSH error: `ssh -T git@github.com` must succeed for the user running the server, and github.com must be in `known_hosts`.
- `repo X: not found` for a repository that exists: the user has no role on it. An owner or admin of its organization grants one: `vx repo grant X USER --role write`.
- Everything else: `docker compose logs vx`.
