# Deploying for a team

One vx server that a team shares. Everyone signs in with their own token, and each person's agents get their own tokens. Each repository belongs to an organization, and people see only the repositories they have a role on. Read [organizations.md](organizations.md) for how roles work and for the current limits.

Prefer to have Claude do it? Install the plugin and ask *"deploy vx on my server vx.example.com for my team"* (skill `vx:deploy`).

## Server

Any Linux VM with 2 vCPU and 4 GB RAM is plenty to start. Disk grows with the repositories you connect; objects are stored once and compressed. Install Docker with the compose plugin and open only ports 80 and 443.

## Install

```sh
git clone https://github.com/TerracodeBA/vx /opt/vx && cd /opt/vx/deploy
umask 077
cat > .env <<EOF
POSTGRES_PASSWORD=$(openssl rand -base64 32)
VX_VERSION=latest
EOF
docker compose up -d
```

To let vx open pull requests on GitHub, add `VX_GITHUB_TOKEN=...` to `.env`: a fine-grained token with **Contents** and **Pull requests** read/write on the repositories the team uses. Without a token, GitHub access goes over SSH from the container and `vx ship open` prints a link to open the PR by hand.

## GitHub App

With a GitHub App, people sign in with GitHub, roles on installed repositories come from GitHub, and webhooks keep repositories, renames and pushes in sync.

1. Set `VX_PUBLIC_URL` (for example `https://vx.example.com`) and `VX_SECRET_KEY` (`openssl rand -hex 32`, once) in `.env`, and restart. `deploy/production/install.sh` does both.
2. As a site admin, open **Admin > GitHub** in the web UI, enter the GitHub organization that should own the App (or leave it empty for your own account) and click **Create the GitHub App**. GitHub shows the App vx asks for (permissions: metadata read, contents, pull requests and checks write, commit statuses read, organization members read; webhook at `/hooks/github`) and sends you back; click **Finish setting up GitHub**.
3. Click **Install the App on GitHub** and pick the organizations and repositories. They show up in vx at once; their roles come from GitHub.

The App subscribes to these webhook events: repository, member, membership, organization, team, team_add, push, pull_request (roles, renames, pushes and merges), and pull_request_review, pull_request_review_comment, issue_comment, check_run, check_suite, status (review comments and CI results reach the agents, see docs/agents.md).

**An App created before v0.4** lacks some of these. Admin > GitHub then says what is missing, for example "Grant Checks: Write in the App's settings". In the App's settings on GitHub (Settings > Developer settings > GitHub Apps > the App > Permissions & events): set **Checks** to Read and write and **Commit statuses** to Read-only, tick the events **Pull request review**, **Pull request review comment**, **Issue comment**, **Check run**, **Check suite** and **Status**, and save. GitHub then asks each installation's owners to accept the new permissions (Settings > GitHub Apps > the App > Review request); until they do, vx skips the `vx evidence` check run on their repositories and their review and CI events do not arrive. Nothing else changes: shipping keeps working.

Repositories of an installation fetch, push `vx/` branches and open pull requests with the App's installation token, so they need neither `VX_GITHUB_TOKEN` nor an SSH key. The App's private key, client secret and webhook secret are stored encrypted with `VX_SECRET_KEY`. Webhooks need `https://vx.example.com/hooks/github` reachable from GitHub; nothing else has to be open.

## GitLab, Gitea, Forgejo and Bitbucket

vx ships to GitLab (gitlab.com and self-managed, merge requests through the REST API v4), Gitea and Forgejo (one adapter, pull requests API) and Bitbucket Cloud (pull requests API 2.0) the way it ships to GitHub: it pushes `vx/` branches over HTTPS with a token and opens or updates the pull request (merge request). Each repository is connected with `vx repo create` (see [Getting started](getting-started.md#4-connect-a-repository)).

**Tokens.** A repository uses its own token when it has one (`vx repo create ... --token-file FILE`, or later `vx repo host NAME --token-file FILE`), stored encrypted with `VX_SECRET_KEY` and never shown again. Otherwise it uses the server's token for its host, `VX_<HOST>_TOKEN`, which vx only sends to the address in `VX_<HOST>_URL`: a repository on another address needs its own token. Without any token vx can read public repositories, and `vx ship open` prints the host's page to open the pull request by hand.

| Host | Token | Scopes |
|---|---|---|
| GitLab | personal, project or group access token | `api` (it covers git over HTTPS too); role Developer or higher on the project |
| Gitea, Forgejo | access token (Settings > Applications) | `write:repository` |
| Bitbucket Cloud | repository, project or workspace access token; or `USERNAME:APP_PASSWORD` | `repository:write`, `pullrequest:write` (app passwords: Repositories write, Pull requests write) |

Git over HTTPS uses the token as the password of `oauth2` (GitLab, Gitea, Forgejo) or `x-token-auth` (Bitbucket access tokens); a Bitbucket app password goes as `USERNAME:APP_PASSWORD` in the token file. Tokens only travel in `Authorization` headers, never in URLs or logs.

**Webhooks** keep vx in sync without polling: a push to the default branch, or to another branch an effort started from, is imported (and efforts whose changes reached it are marked merged), and a merged `vx/` pull request marks its effort merged at once, even after a squash merge. `vx repo create` prints the URL and a webhook secret made for that repository (shown once; `vx repo host NAME --rotate-webhook-secret` makes a new one). Instead of one secret per repository you can set `VX_<HOST>_WEBHOOK_SECRET` on the server and use it in every webhook of that host; a repository's own secret wins. Deliveries without the secret, or with a wrong one, are refused with 401. Register:

| Host | Where | URL | Secret | Events |
|---|---|---|---|---|
| GitLab | Project > Settings > Webhooks > Add new webhook (or a group webhook for many projects) | `https://vx.example.com/hooks/gitlab` | Secret token (sent as `X-Gitlab-Token`) | Push events (all branches), Merge request events |
| Gitea | Repository > Settings > Webhooks > Add webhook > Gitea, POST, `application/json` | `https://vx.example.com/hooks/gitea` | Secret (HMAC-SHA256 in `X-Gitea-Signature`) | Custom events: Push, Pull Request |
| Forgejo, Codeberg | the same, webhook type Forgejo | `https://vx.example.com/hooks/forgejo` (or `/hooks/gitea`) | Secret (`X-Forgejo-Signature`) | Custom events: Push, Pull Request |
| Bitbucket Cloud | Repository settings > Webhooks > Add webhook | `https://vx.example.com/hooks/bitbucket` | Secret (HMAC in `X-Hub-Signature`); a webhook without a secret may use `https://vx.example.com/hooks/bitbucket?secret=SECRET` | Repository: Push; Pull request: Merged |

Deliveries are matched to vx repositories by the project path in the payload (`group/project`, `owner/repo`, `workspace/repo`) and the host's address, so two servers with the same path are kept apart. A delivery is applied once: redeliveries of the same delivery id are acknowledged and skipped.

**Roles.** These repositories use vx's own roles: grants (`vx repo grant`), teams and the organization's default role, as for repositories without a host. Permissions on GitLab, Gitea or Bitbucket are not read; give the people who work there the vx roles they need. Roles from the host, like GitHub-backed repositories have, are a later step.

**Not yet:** review comments and CI results (pipelines, commit statuses) from these hosts do not reach the board yet (the host-neutral `service.ApplyReview` and `service.ApplyCI` are ready for their webhooks), and vx does not read the host's permissions. Pushes and merges from them already rebase open efforts automatically.

## HTTPS

vx listens on 127.0.0.1:7070. Put a reverse proxy with TLS in front of it. With Caddy:

```
vx.example.com {
    reverse_proxy 127.0.0.1:7070
}
```

The CLI and API use HTTP/2 (connect protocol) over the same address, so one hostname serves the web UI, the CLI and agents.

## Users and agents

```sh
docker compose exec vx vx-server user-add alice --admin --org acme --name "Alice Example" --email alice@example.com
docker compose exec vx vx-server user-add bob --org acme --name "Bob Example"
docker compose exec vx vx-server token-create alice      # another token for an existing user
```

`user-add` creates the person, their personal organization (named like them) and a first token.

- `--admin` makes them a site admin: admin on every repository, and allowed to manage people and every organization. Make the first person one.
- `--org ORG` adds them to an organization as a member. When the organization does not exist yet, it is created with them as its owner, which is how the first person sets up the team's organization.

Members get the organization's default role (`read` unless changed) on its repositories. Owners and admins manage the rest with `vx org`, `vx team` and `vx repo grant`; see [organizations.md](organizations.md).

Once signed in, people create further tokens for themselves and their agents with `vx token create` or the Settings page, without an admin. The web UI marks its session cookie `Secure` when the proxy sends `X-Forwarded-Proto: https` (Caddy does by default).

Hand each token to its person privately. People then create their agents themselves:

```sh
curl -fsSL https://vx.example.com/install.sh | sh   # the CLI, from your server, into ~/.local/bin
vx login --server https://vx.example.com            # opens the browser to approve; or add --token <token>
vx agent add backend --env-file ~/.vx-agents/backend.env
```

Agents never log in through the browser; their tokens come from `vx agent add`.

## Production on one server

`deploy/production` turns a fresh Ubuntu 24.04 server into a running vx with one command: Docker, a firewall (SSH, HTTP, HTTPS only), automatic security updates, vx built from a git ref, Caddy with automatic HTTPS, and nightly backups. vx-server never runs customer code (checks run on the agents' machines), so one 2 to 4 core server with 8 GB of RAM carries a lot of agents.

1. Create the server and point the domain's DNS at its address (for Cloudflare, a DNS-only A record, so Caddy can get the certificate itself).
2. On the server, as root:
   ```sh
   curl -fsSLo install.sh https://raw.githubusercontent.com/TerracodeBA/vx/main/deploy/production/install.sh
   less install.sh
   bash install.sh app.example.com
   ```
3. Create the first person: `cd /opt/vx/src/deploy/production && docker compose exec vx vx-server user-add HANDLE --name "Full Name" --email you@example.com`
4. Update later with `bash /opt/vx/src/deploy/production/update.sh [GIT_REF]` (it backs up first).

Backups land in `/var/backups/vx` every night at 03:15 and are kept for 14 days. To also copy them off the server, configure an rclone remote named `offsite` (for example a Cloudflare R2 bucket: `rclone config`); `vx-backup` then copies new backups there.

### Google Cloud

An always-on Compute Engine VM, on the **Standard** network tier (it includes 200 GB of outbound traffic a month; the default Premium tier charges for almost all of it, and agents download a lot):

```sh
P=vxbench R=europe-west1 Z=europe-west1-b
gcloud services enable compute.googleapis.com --project $P
gcloud compute addresses create vx-ip --project $P --region $R --network-tier STANDARD
gcloud compute firewall-rules create vx-web --project $P --allow tcp:80,tcp:443,udp:443 --target-tags vx-web
gcloud compute instances create vx-1 --project $P --zone $Z --machine-type e2-standard-2 \
  --image-family ubuntu-2404-lts-amd64 --image-project ubuntu-os-cloud \
  --boot-disk-size 50GB --boot-disk-type pd-balanced \
  --network-tier STANDARD --address vx-ip --tags vx-web --deletion-protection
gcloud compute ssh vx-1 --project $P --zone $Z -- sudo bash   # then step 2 above
```

### Hetzner

A CAX21 (4 ARM cores, 8 GB) with Ubuntu 24.04 and your SSH key; then step 2 above over `ssh root@SERVER`. Traffic is included.

## Backups

Back up Postgres and the data volume together:

```sh
docker compose exec -T postgres pg_dump -U vx vx | gzip > /backup/vx-$(date +%F).sql.gz
docker run --rm -v deploy_data:/data -v /backup:/backup alpine tar -czf /backup/vx-data-$(date +%F).tgz -C /data .
```

Run it daily and test a restore.

## Upgrades

```sh
cd /opt/vx && git pull && cd deploy
docker compose pull && docker compose up -d
```

Database migrations run when `vx-server` starts. Back up before upgrading.

### Upgrading to v0.2

v0.2 adds organizations and repository roles. The migration keeps everyone's access as it was: each owner part of existing repository names becomes an organization (repositories without one go to `default`), every existing person becomes an owner of all of them, and every existing person becomes a site admin. Prune afterwards, signed in as someone who stays a site admin:

```sh
vx org list --all                              # what the upgrade created
vx org show acme                               # who is in it
vx org member acme bob --role member           # owners only where they should be
vx org remove acme carol                       # people who do not belong
vx org set-default acme none                   # if members should not see every repository
vx repo grant acme/api team:acme/backend --role write
```

Site admin is a flag on the person. Until the admin pages cover it, take it away for everyone except the real admins in Postgres:

```sh
docker compose exec -T postgres psql -U vx vx -c "update principals set site_admin = false where handle not in ('alice')"
```

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `VX_DATABASE_URL` | set by compose | Postgres connection URL |
| `VX_DATA_DIR` | `/data` in the image | Objects and git mirrors |
| `VX_ADDR` | `0.0.0.0:7070` in the image | Listen address |
| `VX_GITHUB_TOKEN` | empty | GitHub API and HTTPS git access |
| `VX_PUBLIC_URL` | empty | This server's public address, like `https://vx.example.com`; needed for the GitHub App (callbacks, webhooks) and the CLI login |
| `VX_SECRET_KEY` | empty | 32 bytes, 64 hex characters or base64 (`openssl rand -hex 32`); seals the GitHub App's credentials and the tokens and webhook secrets of GitLab, Gitea, Forgejo and Bitbucket repositories at rest. Required once a GitHub App exists: the server does not start without it. Keep it with your backups; never change it |
| `VX_SIGNUP` | `invite` | Who may create an account by signing in with GitHub: `invite` (logins a site admin invited), `open` (anyone) or `closed` (nobody new) |
| `VX_GITHUB_URL`, `VX_GITHUB_API_URL` | github.com | GitHub Enterprise Server only: `https://HOST` and `https://HOST/api/v3` (set both) |
| `VX_GITLAB_URL` | `https://gitlab.com` | GitLab address repositories default to (`--gitlab-url` overrides per repository) |
| `VX_GITLAB_TOKEN` | empty | GitLab token for repositories on `VX_GITLAB_URL` |
| `VX_GITEA_URL`, `VX_FORGEJO_URL` | empty | Gitea or Forgejo address repositories default to; without it every repository needs `--gitea-url` or `--forgejo-url` |
| `VX_GITEA_TOKEN`, `VX_FORGEJO_TOKEN` | empty | Token for repositories on that address |
| `VX_BITBUCKET_TOKEN` | empty | Bitbucket Cloud access token, or `USERNAME:APP_PASSWORD` |
| `VX_GITLAB_WEBHOOK_SECRET`, `VX_GITEA_WEBHOOK_SECRET`, `VX_FORGEJO_WEBHOOK_SECRET`, `VX_BITBUCKET_WEBHOOK_SECRET` | empty | Server-wide webhook secret per host, for repositories without their own |
| `VX_<HOST>_API_URL` | derived | API address when it is not the default (`URL/api/v4`, `URL/api/v1`, `https://api.bitbucket.org/2.0`) |
| `POSTGRES_PASSWORD` | `vx` | Database password used by compose |
| `VX_VERSION` | `latest` | Image tag |

## Monitoring

- `GET /healthz` returns `ok` when the server can reach Postgres.
- Logs go to stdout: `docker compose logs -f vx`.
