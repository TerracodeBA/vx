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

Every vx server serves an installer for the CLI:

```sh
curl -fsSL http://127.0.0.1:7070/install.sh | sh
```

It downloads `vx` for your OS and CPU from the [latest release](https://github.com/TerracodeBA/vx/releases), checks it against the release's `checksums.txt`, installs it into `~/.local/bin` and tells you whether that directory is on your `PATH`. If it is not, add it, for example for bash:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc   # then open a new terminal
```

`VX_VERSION=v0.3.0` picks a release, `VX_INSTALL_DIR=/somewhere/bin` another directory. With Go 1.26+ you can also `go install github.com/TerracodeBA/vx/cmd/vx@latest`.

Then log in:

```sh
vx login --server http://127.0.0.1:7070
vx whoami                                     # user:alice
```

`vx login` opens the server's approval page in your browser and prints a code such as `WDJB-MJHT`. Sign in on that page (with GitHub when the server has it set up, or with the token from step 2), check that it shows the same code, and click **Approve**. vx saves a new token named "vx on HOST (browser login)" in `~/.config/vx/config.json`; you can revoke it on the web UI's Settings page. The code works once, for ten minutes.

- On a machine without a browser, `vx login --server URL --no-browser` only prints the link: open it on any device.
- `vx login --server URL --token TOKEN` (or `VX_TOKEN`) logs in with a token instead, for example the one from step 2.
- Agents never log in through a browser: they get their own token from `vx agent add` (step 6).

**Several servers.** Each `vx login --server URL` saves a profile named after the server's host (`app.vxbench.com`, `127.0.0.1:7070`) and makes it current; `--profile NAME` names it yourself. Logging in again to the same server refreshes its profile.

```sh
vx login --server https://app.vxbench.com     # profile app.vxbench.com, now current
vx login --server http://127.0.0.1:7070       # profile 127.0.0.1:7070, now current
vx profiles                                   # both, the current one marked with *
vx use app.vxbench.com                        # switch (a profile name, or the server URL or host)
vx --profile 127.0.0.1:7070 whoami            # one command with another profile
VX_PROFILE=127.0.0.1:7070 vx log              # the same, from the environment
vx profiles remove 127.0.0.1:7070             # forget one (revoke its token on the server if it should stop working)
```

`VX_SERVER` and `VX_TOKEN` (an agent's env file, for example) still take precedence over every profile, and `VX_CONFIG` still picks the config file, so agents keep using their own token. `--profile` together with `VX_TOKEN` is refused, since vx cannot tell which login you mean. A config file from before profiles (one server and token) keeps working: the first command turns it into a profile named after its host.

vx checks the server's version when you log in and about once a day after that. When the server is newer it prints a one-line notice with the update command; a server that no longer works with your vx refuses it with the same command. Updating is running the installer again.

## 4. Connect a repository

```sh
vx repo create acme/shop-api --github acme/shop-api   # GitHub
vx repo create acme/shop --gitlab acme/shop            # GitLab (gitlab.com)
vx repo create acme/shop --gitlab acme/platform/shop --gitlab-url https://gitlab.example.com
vx repo create acme/shop --gitea acme/shop --gitea-url https://gitea.example.com
vx repo create acme/shop --forgejo acme/shop --forgejo-url https://codeberg.org
vx repo create acme/shop --bitbucket acme/shop         # Bitbucket Cloud: workspace/repo
vx repo create shop --git /path/or/url/to/repo         # any git remote or local path
vx repo create scratch                                 # vx only, no upstream
```

vx imports the default branch right away. How it reaches GitHub depends on the server:

- **Without `VX_GITHUB_TOKEN`**: over SSH with the server user's keys. In Docker, that means the container needs SSH access; for a personal setup it is often simpler to run `vx-server` directly (see below).
- **With `VX_GITHUB_TOKEN`** (in `deploy/.env`): over HTTPS, and vx opens pull requests itself. Use a fine-grained token with Contents and Pull requests read/write.

### GitLab, Gitea, Forgejo and Bitbucket

vx reaches these over HTTPS with a token and opens merge requests (GitLab) or pull requests itself. Give the repository its own token, or let it use the server's (`VX_GITLAB_TOKEN`, `VX_GITEA_TOKEN`, `VX_FORGEJO_TOKEN`, `VX_BITBUCKET_TOKEN`, see [Deploying](deploy.md#gitlab-gitea-forgejo-and-bitbucket)):

```sh
printf '%s' "$GITLAB_TOKEN" > ~/.gitlab-token && chmod 600 ~/.gitlab-token
vx repo create acme/shop --gitlab acme/platform/shop --gitlab-url https://gitlab.example.com --token-file ~/.gitlab-token
```

The token is stored encrypted on the server and never shown again; `--token-file -` reads it from stdin, and a Bitbucket app password goes in as `USERNAME:APP_PASSWORD`. GitLab tokens need the `api` scope, Gitea and Forgejo tokens `write:repository`, Bitbucket access tokens `repository:write` and `pullrequest:write`.

`vx repo create` then prints the webhook to register on the host, with a secret made for this repository (shown once):

```
Register the webhook on GitLab: Settings > Webhooks > Add new webhook:
  URL:     https://vx.example.com/hooks/gitlab
  Secret token: 3f9c...
  Events:  Push events, Merge request events, Comments, Pipeline events
```

With the webhook, pushes to the default branch are imported right away, a merged `vx/` merge request marks its effort merged, and review comments and pipeline results reach the agents. Without it, run `vx repo sync` now and then. Later changes: `vx repo host acme/shop --token-file FILE` replaces the token, `--clear-token` goes back to the server's, `--rotate-webhook-secret` makes a new secret. `vx repo list` and the repository page in the web UI show the host.

Roles on these repositories are vx's own: grant people access with `vx repo grant` (or the organization's default role), whatever they can do on GitLab, Gitea or Bitbucket.

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

Run `vx ui` to open http://127.0.0.1:7070 already signed in (or sign in there yourself). The effort page updates live as agents land.

When the work is ready:

```sh
vx ship plan --repo acme/shop-api rate-limiting   # the commits and PR text
vx ship open --repo acme/shop-api rate-limiting --draft
```

## Signing in

On a server with GitHub sign-in (a site admin set up the GitHub App on Admin > GitHub, and `VX_PUBLIC_URL` is the address people open), people sign in with GitHub. There are no tokens to hand around:

1. Open the server (for example https://app.vxbench.com) and choose **Sign in with GitHub**. Approve vx on GitHub. The first time, this creates your vx account: your handle is your GitHub login in lowercase (with `-2`, `-3`... when someone already has it), your email is GitHub's primary verified email, or your GitHub no-reply address when you do not share one.
2. vx opens **Set up vx** with three steps: install the vx GitHub App on your organization, install the CLI (`curl -fsSL https://app.vxbench.com/install.sh | sh`), and `vx login --server https://app.vxbench.com`, which opens the browser for you to approve. Then, in Claude Code, `/plugin marketplace add TerracodeBA/vx` and `/plugin install vx@vx`. The page stays in the menu until you hide it.

Who may create an account is the server's sign-up mode, `VX_SIGNUP`:

- `invite` (the default): only invited GitHub logins. A site admin invites on **Admin > People > Invite by GitHub login**, or on the server console with `vx-server invite LOGIN --note "why"`. The invite is used up on the first sign-in. The first admin of a new server: `vx-server invite LOGIN --admin`, then sign in with GitHub.
- `open`: anyone with a GitHub account.
- `closed`: nobody new; people who have an account still sign in.

Someone who is not invited gets a page that says so and names the GitHub login to invite. People created earlier with `vx-server user-add` keep their account: signed in with their token, they choose **Settings > Connect GitHub** once, and sign in with GitHub from then on. vx links people by GitHub's numeric user id, so renaming a GitHub login keeps the account.

**The vxbench.com waitlist** is separate from the server: it collects requests and creates no accounts. To let someone in from it, take the GitHub login from their waitlist entry (ask them for it if they left only an email), invite it as above with the note `waitlist`, and tell them to open https://app.vxbench.com and choose Sign in with GitHub.

Without GitHub sign-in, sign in with a token (step 2 above) or run `vx ui`, which opens the web UI already signed in.

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
| `VX_GITLAB_TOKEN`, `VX_GITEA_TOKEN`, `VX_FORGEJO_TOKEN`, `VX_BITBUCKET_TOKEN` | empty | Server-wide token per host (only sent to `VX_<HOST>_URL`; GitLab defaults to gitlab.com) |

CLI environment: `VX_SERVER`, `VX_TOKEN` (both take precedence over saved profiles), `VX_CONFIG` (config file path), `VX_PROFILE` (the saved profile to use, like `--profile`), `VX_REPO` (default repo), `VX_MODEL` and `VX_SESSION` (recorded on each change).
