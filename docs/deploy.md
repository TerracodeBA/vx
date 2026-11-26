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
2. As a superadmin, open **Admin > GitHub** in the web UI, enter the GitHub organization that should own the App (or leave it empty for your own account) and click **Create the GitHub App**. GitHub shows the App vx asks for (permissions: metadata read, contents, pull requests and checks write, commit statuses read, organization members read; webhook at `/hooks/github`) and sends you back; click **Finish setting up GitHub**.
3. Click **Install the App on GitHub** and pick the organizations and repositories. They show up in vx at once; their roles come from GitHub.

The App subscribes to these webhook events: repository, member, membership, organization, team, team_add, push, pull_request (roles, renames, pushes and merges), and pull_request_review, pull_request_review_comment, issue_comment, check_run, check_suite, status (review comments and CI results reach the agents, see docs/agents.md).

**An App created before v0.4** lacks some of these. Admin > GitHub then says what is missing, for example "Grant Checks: Write in the App's settings". In the App's settings on GitHub (Settings > Developer settings > GitHub Apps > the App > Permissions & events): set **Checks** to Read and write and **Commit statuses** to Read-only, tick the events **Pull request review**, **Pull request review comment**, **Issue comment**, **Check run**, **Check suite** and **Status**, and save. GitHub then asks each installation's owners to accept the new permissions (Settings > GitHub Apps > the App > Review request); until they do, vx skips the `vx evidence` check run on their repositories and their review and CI events do not arrive. Nothing else changes: shipping keeps working.

Repositories of an installation fetch, push `vx/` branches and open pull requests with the App's installation token, so they need neither `VX_GITHUB_TOKEN` nor an SSH key. The App's private key, client secret and webhook secret are stored encrypted with `VX_SECRET_KEY`, and so are the client secrets of organizations' [single sign-on](organizations.md#single-sign-on) (which also needs `VX_PUBLIC_URL`: providers send people back to `VX_PUBLIC_URL/auth/oidc/callback`). Webhooks need `https://vx.example.com/hooks/github` reachable from GitHub; nothing else has to be open.

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
| GitLab | Project > Settings > Webhooks > Add new webhook (or a group webhook for many projects) | `https://vx.example.com/hooks/gitlab` | Secret token (sent as `X-Gitlab-Token`) | Push events (all branches), Merge request events, Comments, Pipeline events |
| Gitea | Repository > Settings > Webhooks > Add webhook > Gitea, POST, `application/json` | `https://vx.example.com/hooks/gitea` | Secret (HMAC-SHA256 in `X-Gitea-Signature`) | Custom events: Push, Pull Request, Pull Request Reviewed, Pull Request Comment (Issue Comment on older versions), Commit Status |
| Forgejo, Codeberg | the same, webhook type Forgejo | `https://vx.example.com/hooks/forgejo` (or `/hooks/gitea`) | Secret (`X-Forgejo-Signature`) | Custom events: Push, Pull Request, Pull Request Reviewed, Pull Request Comment, Commit Status |
| Bitbucket Cloud | Repository settings > Webhooks > Add webhook | `https://vx.example.com/hooks/bitbucket` | Secret (HMAC in `X-Hub-Signature`); a webhook without a secret may use `https://vx.example.com/hooks/bitbucket?secret=SECRET` | Repository: Push, Build status created and updated; Pull request: Merged, Approved, Changes request created, Comment created and updated |

Deliveries are matched to vx repositories by the project path in the payload (`group/project`, `owner/repo`, `workspace/repo`) and the host's address, so two servers with the same path are kept apart. A delivery is applied once: redeliveries of the same delivery id are acknowledged and skipped.

**Roles.** These repositories use vx's own roles: grants (`vx repo grant`), teams and the organization's default role, as for repositories without a host. Permissions on GitLab, Gitea or Bitbucket are not read; give the people who work there the vx roles they need. Roles from the host, like GitHub-backed repositories have, are a later step.

Review comments, approvals and CI results (GitLab pipelines, Gitea and Forgejo commit statuses, Bitbucket build statuses) on vx pull requests reach the agents whose changes they are about, as on GitHub (see [reviews and CI from the git host](agents.md#reviews-and-ci-from-the-git-host)); pushes and merges rebase open efforts automatically. **Not yet:** vx does not read these hosts' permissions, and posts no `vx evidence` status on their pull requests.

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

- `--admin` makes them a superadmin: they run the server (people, invites, the GitHub App, server settings, the operation log's metadata) but have no role on any repository from it. Make the first person one, and give them `--org` too so they own the team's organization and its repositories.
- Later, without SQL: `vx-server superadmin grant HANDLE`, `vx-server superadmin revoke HANDLE` and `vx-server superadmin list` (each change goes to the operation log as `person.superadmin_grant` or `person.superadmin_revoke`, from the console).
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

Backups land in `/var/backups/vx` every night at 03:15 and are kept for 14 days. Set up off-site copies too (see [Backups](#backups)): a backup on the same disk does not survive losing the server.

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

`deploy/production` installs `vx-backup` (`backup.sh`), run every night at 03:15 and before every update. It writes to `/var/backups/vx`, kept 14 days (at most 20 of each kind):

- `vx-db-STAMP.dump`: the Postgres database (`pg_dump --format=custom`).
- `vx-data-STAMP.tgz`: the data volume, while objects live on it (no `VX_BLOB_URL`). With objects in a bucket the volume only holds git mirrors, which vx fetches again, so it is skipped.

Files are written as `.part` and renamed when complete, so a failed run never leaves a backup that looks whole.

### Off-site copies

Put a bucket in `/opt/vx/.env`, then run `docker compose up -d` once in `/opt/vx/src/deploy/production` so the vx container sees it:

```sh
VX_BACKUP_URL=s3://vx-backups/app.example.com?region=auto&endpoint=https://ACCOUNT.r2.cloudflarestorage.com
VX_BACKUP_ACCESS_KEY_ID=...
VX_BACKUP_SECRET_ACCESS_KEY=...
VX_BACKUP_KEEP_DAYS=30        # optional; 0 leaves deleting to a lifecycle rule
```

Every backup is then also uploaded there (large files in 16 MB parts, retried on errors) with a `.sha256` next to it, and backups older than `VX_BACKUP_KEEP_DAYS` are deleted from the bucket (the newest of each kind always stays). The URL takes the same forms as `VX_BLOB_URL` (AWS S3, R2, MinIO, Google Cloud Storage with HMAC keys; see [Object storage](#object-storage)). A run whose upload fails exits with an error after keeping the local copy; `/var/log/vx-backup.log` shows it.

Safer still: give the backup key permission to write but not delete, protect the bucket with versioning or object lock, set `VX_BACKUP_KEEP_DAYS=0` and let a lifecycle rule expire old backups. Keep `VX_SECRET_KEY` somewhere else than the backups (a password manager): a restore needs it, and anyone with both can open the stored GitHub App and host credentials.

`vx-server backup list|get|put|prune` work on the same bucket by hand. An rclone remote named `offsite` (the v0.4 way) still gets a copy as well.

### Restore

`vx-restore` (`restore.sh`) brings a backup back, on the same server or on a fresh one:

1. Set up the new server with `install.sh` (step 2 of [Production on one server](#production-on-one-server)).
2. Put the old server's `VX_SECRET_KEY` into `/opt/vx/.env`, and its `VX_BACKUP_*` and `VX_BLOB_*` settings if it had them, then `cd /opt/vx/src/deploy/production && docker compose up -d`.
3. `vx-restore` lists the backups, local and off-site; `vx-restore latest` (or `vx-restore 2026-11-24-0315`) restores one. Backups not on the server are downloaded and checked against their `.sha256`. It saves the current database as `vx-before-restore-*.dump`, stops vx, replaces the database (and the data volume when the backup has an archive of it), starts vx and prints what it restored.
4. Point DNS at the new server. Git mirrors fill again on first use.

A backup with objects on disk restored onto a server that reads them from a bucket needs one more step, which `vx-restore` prints: `docker compose exec vx vx-server blobs copy --from file:///data/objects --to "$VX_BLOB_URL"`.

### Test the restore

Restore into a scratch server now and then, not only on the day you need it: a second VM, steps 1 to 3 above with `VX_RESTORE_YES=1`, then sign in and open a repository. `deploy/production/backup_test.sh` (`make test-backup`) does exactly that on one machine with Docker: it starts Postgres, MinIO and vx-server from the source tree, lands a file, runs `backup.sh`, deletes the database, the data volume and the local backups, restores from MinIO with `restore.sh` on a fresh database and reads the file back.

## Object storage

By default objects (file contents and trees) live on the data volume, in `/data/objects`. With `VX_BLOB_URL` they live in an S3-compatible bucket instead, so the server keeps nothing it cannot rebuild, and several servers can share one bucket.

| Service | `VX_BLOB_URL` |
|---|---|
| AWS S3 | `s3://BUCKET/PREFIX?region=eu-central-1` |
| Cloudflare R2 | `s3://BUCKET/PREFIX?region=auto&endpoint=https://ACCOUNT.r2.cloudflarestorage.com` |
| MinIO | `s3://BUCKET/PREFIX?endpoint=http://minio:9000` |
| Google Cloud Storage | `gs://BUCKET/PREFIX` (S3 interoperability with HMAC keys: Cloud Storage > Settings > Interoperability) |

- Credentials: `VX_BLOB_ACCESS_KEY_ID` and `VX_BLOB_SECRET_ACCESS_KEY` (and `VX_BLOB_SESSION_TOKEN` for temporary ones), each also as `..._FILE`, a file holding the value (Docker or Kubernetes secrets); `AWS_ACCESS_KEY_ID` and friends are the fallback. Never in the URL: vx refuses URLs with credentials, and keys never appear in logs or errors.
- Parameters: `region` (signs requests; default `us-east-1`), `endpoint` (any S3-compatible service), `style=path|virtual` (default: virtual-host style on AWS, path style with an `endpoint`), `cache=64MB` (memory for hot objects in front of the bucket, least recently used out, objects over an eighth of it are not cached; `cache=0` turns it off).
- The key needs `GetObject`, `PutObject` and `ListBucket` on the bucket (without `ListBucket`, AWS answers 403 instead of 404 for missing objects). vx never deletes objects.
- Every request is signed (SigV4) and retried with backoff on network errors, 5xx and 429; existence checks are `HEAD` requests; every object read is checked against its hash.
- The layout is the disk store's (`PREFIX/9f/1c/9f1c...`, same bytes), so a copy of `/data/objects` made with any tool is a valid bucket, and the other way round.
- Native Google Cloud authentication (service accounts, workload identity) and AWS instance roles are not supported yet: use HMAC or access keys.

### Moving to object storage

`vx-server blobs copy --to URL` copies every object of the current store into the bucket. It checks each object's hash on read, reads every copy back from the bucket, skips objects the bucket already has (so an interrupted copy resumes: run it again) and prints what to set. Destination credentials come from `VX_BLOB_TO_ACCESS_KEY_ID` and `VX_BLOB_TO_SECRET_ACCESS_KEY`, else `VX_BLOB_*`.

1. Create the bucket and a key for it. Add `VX_BLOB_ACCESS_KEY_ID` and `VX_BLOB_SECRET_ACCESS_KEY` (not `VX_BLOB_URL` yet) to `/opt/vx/.env` and run `docker compose up -d` in `/opt/vx/src/deploy/production`, so the container has the key.
2. Copy while vx runs (this takes the longest):
   ```sh
   cd /opt/vx/src/deploy/production
   URL='s3://vx-objects/prod?region=auto&endpoint=https://ACCOUNT.r2.cloudflarestorage.com'
   docker compose exec vx vx-server blobs copy --to "$URL"
   ```
3. Stop vx and copy again; only what was written meanwhile is copied:
   ```sh
   docker compose stop vx
   docker compose run --rm --no-deps vx blobs copy --to "$URL"
   ```
4. Add `VX_BLOB_URL=$URL` to `/opt/vx/.env` and `docker compose up -d vx`. The log line `blob store store=s3://...` confirms it.

The second copy with vx stopped is what makes switching safe: no object can be written to the old store after it. Keep `/data/objects` until the server has run on the bucket for a while; `--verify-existing` reads back every object already in the bucket too. Moving back works the same way, with `--to file:///data/objects`.

Objects in a bucket are immutable and never deleted by vx, so nightly backups skip them: protect the bucket at the provider (versioning, replication), or copy it to a second bucket now and then with `vx-server blobs copy --from s3://... --to s3://...`.

Storage used per repository and organization (for quotas) is recorded in Postgres (`object_sizes`): new uploads and merges within seconds, imports and older objects by a background pass at start and every 10 minutes.

## Several servers

With objects in a bucket, vx-server processes can share one Postgres and one bucket behind a load balancer, with no sticky sessions:

- Shared: everything in Postgres (efforts, changes, notes, tokens, web sign-in codes, CLI logins) and the objects in the bucket. Lands are compare-and-swap on the effort, branch imports only move a branch from the commit they started at, migrations take a lock, overlap notes take a lock per pair.
- Local and rebuildable: git mirrors in `VX_DATA_DIR/git`, one bare repository per repository. A server without them (a new replica, an emptied volume) fetches on first use, and a mirror that no longer opens is set aside and fetched again. Give each server its own directory; do not share it over NFS. The first fetch on a new server takes as long as an import did.
- Local and harmless: the memory cache of objects, cached GitHub installation tokens, and per-process locks that keep one server from fetching one repository twice at once.

Not done yet: a background job (an object backfill or an overlap forecast) may run on two servers at once; both are idempotent, so this costs time, not correctness.

## Upgrades

```sh
cd /opt/vx && git pull && cd deploy
docker compose pull && docker compose up -d
```

Database migrations run when `vx-server` starts. Back up before upgrading.

### Upgrading to v0.2

v0.2 adds organizations and repository roles. The migration keeps everyone's access as it was: each owner part of existing repository names becomes an organization (repositories without one go to `default`), every existing person becomes an owner of all of them, and every existing person becomes a site admin (since v0.5 a superadmin: see below). Prune afterwards, signed in as someone who stays a superadmin:

```sh
vx org list --all                              # what the upgrade created
vx org show acme                               # who is in it
vx org member acme bob --role member           # owners only where they should be
vx org remove acme carol                       # people who do not belong
vx org set-default acme none                   # if members should not see every repository
vx repo grant acme/api team:acme/backend --role write
```

Superadmin (the `site_admin` column) is a flag on the person. Take it away for everyone except the people who run the server, on the console (`vx-server superadmin revoke HANDLE`) or in Postgres:

```sh
docker compose exec -T postgres psql -U vx vx -c "update principals set site_admin = false where handle not in ('alice')"
```

### Upgrading to v0.5: superadmins

Until v0.5 a site admin was admin on every repository on the server. From v0.5 the flag means **superadmin**: someone who runs the server (Admin, People and GitHub; invites and the sign-up mode; server settings; the operation log's metadata) and has **no role on any repository** from it. To a superadmin, a repository they have no role on is "not found" in the CLI, the API, the web UI and MCP, like to anyone else. See [organizations.md](organizations.md#superadmins-and-support-access).

The upgrade (migration 029) keeps the flag: everyone who was a site admin can still use the admin panel. The old "admin everywhere" access is not carried over. What a site admin could do only because of the flag stops working at once: reading repositories of organizations they are not in, managing organizations they do not own or administer, creating repositories there.

Upgrading from v0.1 made every existing person both an owner of every organization and a site admin (migrations 009 and 010), so most people keep their access through organization ownership. Check before and after:

1. **The notice.** When a server with site admins starts on v0.5 for the first time, it logs once: `upgrade notice: Site admins are now superadmins: ... no longer have access to repositories. Grant support access per repository if needed ...`. Admin, People and Admin, GitHub show the same as a banner ("site admins no longer have access to repositories; grant support access per repository if needed") until a superadmin clicks **Mark as reviewed** (recorded in the operation log as `server.notice.resolve`). A new server, without site admins when the migration runs, gets neither.
2. **Who relied on it.** Each superadmin runs `vx repo list` and `vx org list`. A repository they need and no longer see needs an ordinary role from its organization's owners (`vx org member ORG HANDLE --role member`, `vx repo grant REPO user:HANDLE --role write`), or, for an operator helping a customer, support access.
3. **Support access.** A repository's admins give a superadmin read or write on that one repository for a limited time: `vx repo support REPO HANDLE --role read --for 24h --reason "why"`, or **Support access** on the repository's access page. It ends by itself (at most 30 days), shows as "operator (support access) until DATE", is in the operation log, and the admins end it with `--revoke` or End now.
4. **Fewer superadmins.** Keep only the people who run the server: `vx-server superadmin revoke HANDLE` on the console (or the SQL above).

Organizations whose only owners were removed or left can no longer be fixed by a superadmin from the web UI. Make someone an owner on the console: `docker compose exec -T postgres psql -U vx vx -c "insert into org_members (org_id, person_id, role) select o.id, p.id, 'owner' from orgs o, principals p where o.handle = 'acme' and p.handle = 'alice' on conflict (org_id, person_id) do update set role = 'owner'"`.

## Kubernetes (Helm)

`deploy/helm/vx` deploys vx-server to Kubernetes: a Deployment with health probes and resource defaults, a Service, an optional Ingress, a volume for the data directory, and a ServiceMonitor for the Prometheus Operator if you want one. Postgres is not part of the chart: use a managed database or your own, and give its URL in a Secret.

```sh
kubectl create secret generic vx-db --from-literal=database-url='postgres://vx:PASSWORD@db.example.com:5432/vx?sslmode=require'
kubectl create secret generic vx-secrets \
  --from-literal=secret-key="$(openssl rand -hex 32)" \
  --from-literal=metrics-token="$(openssl rand -hex 24)"
helm install vx deploy/helm/vx \
  --set database.existingSecret=vx-db \
  --set secrets.existingSecret=vx-secrets \
  --set publicURL=https://vx.example.com \
  --set ingress.enabled=true --set ingress.hosts[0].host=vx.example.com
kubectl exec deploy/vx -- vx-server user-add alice --admin --name "Alice Example"
```

What the values cover (see `values.yaml` for all of them):

| Value | Meaning |
|---|---|
| `database.existingSecret`, `database.urlKey` | The Secret and key with the Postgres URL (`VX_DATABASE_URL`). Required. |
| `secrets.existingSecret` | One Secret with `secret-key` (`VX_SECRET_KEY`), `github-token` (`VX_GITHUB_TOKEN`) and `metrics-token` (`VX_METRICS_TOKEN`); missing keys leave the setting off. |
| `publicURL`, `signup` | `VX_PUBLIC_URL` and `VX_SIGNUP`. |
| `blobStore.url`, `blobStore.existingSecret` | Object storage (`VX_BLOB_URL`) and a Secret whose keys become environment variables, such as `VX_BLOB_ACCESS_KEY_ID` and `VX_BLOB_SECRET_ACCESS_KEY` (see [Object storage](#object-storage)). Empty: objects stay on the data volume. |
| `persistence.*` | The data volume (git mirrors; objects without object storage), 20 GiB by default. With a ReadWriteOnce volume the chart replaces the pod with `Recreate`. |
| `rateLimits.*`, `quotas.*`, `auditRetention` | The `VX_RATE_*`, `VX_QUOTA_*` and `VX_AUDIT_RETENTION` settings below. |
| `ingress.*` | Host, class, TLS and annotations. The CLI speaks HTTP/2 through the same host; lands upload up to 64 MiB per request, so raise body size limits (ingress-nginx: `nginx.ingress.kubernetes.io/proxy-body-size: "70m"`). |
| `metrics.serviceMonitor.enabled` | A ServiceMonitor that scrapes `/metrics` with the metrics token. |
| `resources`, `probes.*` | 250m CPU and 512 MiB requested, 2 GiB memory limit. The startup probe allows 10 minutes for migrations on an upgrade. |

Run one replica: rate limits, overlap forecasts and git mirrors live in the process. The pod runs as user 10001 with a read-only root file system and no service account token.

`deploy/helm/chart_test.go` runs `helm lint --strict` and `helm template` on the chart when `helm` is installed (or `VX_HELM` names it), and skips otherwise; it never talks to a cluster.

## Metrics

Set `VX_METRICS_TOKEN` (`openssl rand -hex 24`) and `GET /metrics` serves Prometheus metrics to callers that send it as a bearer token; without the variable the endpoint does not exist (404). A scrape configuration:

```yaml
scrape_configs:
  - job_name: vx
    scheme: https
    authorization: {credentials_file: /etc/prometheus/vx-metrics-token}
    static_configs: [{targets: [vx.example.com]}]
```

| Metric | Labels | What |
|---|---|---|
| `vx_rpc_requests_total`, `vx_rpc_duration_seconds` | `procedure`, `status` | API calls of the CLI, agents and MCP, by procedure (`/vx.v1.WorkspaceService/Land`) and HTTP status (429 is a rate limit) |
| `vx_http_requests_total`, `vx_http_request_duration_seconds` | `route`, `method`, `status` | Web UI, webhooks and the rest, by route pattern (`GET /ui/repos/{path...}`) |
| `vx_lands_total` | `result` | landed, conflicts, review, effort_moved, refused, error |
| `vx_land_conflict_files_total` | | files that conflicted in lands |
| `vx_rebases_total` | `event` | started, done, conflicts, aborted |
| `vx_forecasts_total`, `vx_forecast_duration_seconds` | `result` | background overlap forecasts |
| `vx_operations_total` | `op` | every operation log entry (`effort.create`, `ship`, `org.member`, ...) |
| `vx_queue_depth` | `queue` | forecast (repositories with forecasts queued), object_backfill |
| `vx_webhook_deliveries_total` | `host`, `status` | deliveries from GitHub, GitLab, Gitea, Forgejo, Bitbucket |
| `vx_db_pool_connections`, `vx_db_pool_acquires_total`, `vx_db_pool_acquire_wait_seconds_total`, `vx_db_pool_empty_acquires_total` | `state` | the Postgres connection pool |
| `vx_blob_ops_total`, `vx_blob_op_duration_seconds`, `vx_blob_bytes_total` | `op`, `result` | object store reads and writes |
| `vx_rate_limited_total` | `class` | requests refused by a rate limit |
| `vx_quota_refused_total` | `quota` | requests refused by an organization quota |
| `vx_audit_pruned_total` | | operation log entries deleted by the retention |
| `vx_build_info` | `version`, `go_version` | always 1 |

Labels never carry repository, organization or person names. Each process counts on its own; scrape every replica.

## Rate limits and quotas

**Rate limits** are token buckets per process: a client may make a burst of requests, then as many as the rate refills. They are on by default with limits a team does not notice:

| Variable | Default | Limits |
|---|---|---|
| `VX_RATE_API_TOKEN` | `30/s,burst=300` | API and MCP calls with one token |
| `VX_RATE_API_IP` | `100/s,burst=1000` | API and MCP calls from one address, with any token or none |
| `VX_RATE_SIGNIN` | `30/m` | sign-in pages and callbacks (`/ui/login...`, `/auth/...`: tokens, GitHub, single sign-on), per address |
| `VX_RATE_SIGNUP` | `10/h,burst=5` | new accounts from sign-in, per address |
| `VX_RATE_CLI_LOGIN` | `10/m` | `vx login` started or approved, per address |
| `VX_RATE_WEBHOOK` | `20/s,burst=200` | webhook deliveries, per address |

Write a rate as `N/s`, `N/m` or `N/h`, optionally with `,burst=B` (default: N), or `off`. `VX_RATE_LIMITS=off` turns every limit off. A refused API call fails with `ResourceExhausted`, other requests with 429; both carry `Retry-After` and say `slow down: too many requests with this token; retry in 3 s`. The address is the client's, also behind a proxy on a private address (the last public address in `X-Forwarded-For`). Behind a load balancer each replica counts on its own.

**Quotas** are per organization: repositories, stored data, the size of one import and of one upload, and open efforts. The variables set the defaults for every organization; superadmins override them per organization on its page or with `vx-server quota ORG` ([organizations.md](organizations.md#quotas)).

| Variable | Example | Quota |
|---|---|---|
| `VX_QUOTA_REPOS` | `50` | repositories per organization |
| `VX_QUOTA_STORED` | `20GB` | stored data per organization |
| `VX_QUOTA_IMPORT` | `2GB` | one repository imported from a git host; vx asks the host for its size first and downloads nothing when it is too big |
| `VX_QUOTA_PUSH` | `500MB` | one upload of objects |
| `VX_QUOTA_OPEN_EFFORTS` | `200` | efforts open or shipping per organization |

Empty or `0` means no limit, which is the default. Sizes take `KB`, `MB`, `GB` and `TB` (powers of 1024).

**Disk space.** Superadmins see a warning on every page when the data directory's disk has less than `VX_DISK_WARN_PERCENT` (default 10) percent free, and the free space on Admin > Audit.

## Data processing agreement

A server that offers vx as a service (vx Cloud) can ask every organization to accept its data processing agreement before it is used for work: an owner of the organization reads the agreement in the web UI and accepts it ([organizations.md](organizations.md#data-processing-agreement)). The step is **off by default**: a self-hosted server never shows it.

vx never ships or writes agreement text. You, the operator, publish the versions as files in a directory:

```
/etc/vx/dpa/
  manifest.json
  2026-12.md
  2027-06.md
```

```json
{
  "versions": [
    {"id": "2026-12", "file": "2026-12.md", "published": "2026-12-01", "effective": "2026-12-01", "title": "Data Processing Agreement"},
    {"id": "2027-06", "file": "2027-06.md", "published": "2027-05-01", "effective": "2027-06-01"}
  ]
}
```

- **One Markdown file per version**, UTF-8, at most 1 MiB, in the same directory. The page renders it as Markdown (headings, lists, emphasis, links, tables); raw HTML in it is left out. vx shows its SHA-256 next to it and records it with each acceptance.
- **`id`**: 1 to 64 letters, digits, dots, dashes or underscores, unique. **`published`** and **`effective`**: dates (`YYYY-MM-DD`, midnight UTC) or RFC 3339 times. **`title`** is optional.
- **A new version is published at least 30 days before it takes effect.** From its publication date owners see it (a banner, the For you page, a push notification if they turned notifications on); until its effective date the earlier acceptance still counts; from then an organization that has not accepted it is refused work. The first version may take effect when it is published: there is nobody to announce it to.
- **Never edit a published file.** A changed text is a new version with a new id. The server refuses to start when the text of a version differs from the one owners accepted under that id. Accepted texts are kept in the database, so they stay readable however the files change.
- Add a version by adding its file and its entry, then restart the server (or roll out a new revision). Old versions can stay listed; removing one leaves its acceptances and accepted texts in place.

Turn it on with:

| Variable | Meaning |
|---|---|
| `VX_REQUIRE_DPA` | `1` (or `true`) asks every organization to accept the data processing agreement. Empty or `0`: off, the default; `VX_DPA_DIR` is then not read. |
| `VX_DPA_DIR` | The directory with `manifest.json` and the version files; mount it read-only. |

With `VX_REQUIRE_DPA=1` the server refuses to start, with a message that says why, when `VX_DPA_DIR` is not set, the manifest is missing or invalid (a missing or empty file, a duplicate id or effective date, a version published less than 30 days before it takes effect), or no version is in effect yet. With Helm, set both through `extraEnv`; the chart has no setting to mount the directory yet, so add the volume (a ConfigMap works) to the Deployment yourself, or build an image with the files in it.

Each acceptance is kept in the database for as long as the organization exists, independent of `VX_AUDIT_RETENTION` (which only prunes the `org.dpa_accept` entries of the operation log).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `VX_DATABASE_URL` | set by compose | Postgres connection URL |
| `VX_DATA_DIR` | `/data` in the image | Git mirrors, and objects unless `VX_BLOB_URL` is set |
| `VX_BLOB_URL` | empty (objects in `VX_DATA_DIR/objects`) | Object storage: `s3://BUCKET/PREFIX?region=&endpoint=&style=&cache=` or `gs://BUCKET/PREFIX` ([Object storage](#object-storage)) |
| `VX_BLOB_ACCESS_KEY_ID`, `VX_BLOB_SECRET_ACCESS_KEY`, `VX_BLOB_SESSION_TOKEN` | empty | Bucket credentials; each also as `..._FILE`; `AWS_*` is the fallback |
| `VX_BACKUP_URL` | empty | Bucket for off-site backups (`backup.sh`, `restore.sh`, `vx-server backup`) |
| `VX_BACKUP_ACCESS_KEY_ID`, `VX_BACKUP_SECRET_ACCESS_KEY` | empty | Its credentials; each also as `..._FILE` |
| `VX_BACKUP_KEEP_DAYS` | `30` | Days `backup.sh` keeps backups in the bucket; `0`: never delete (a lifecycle rule does) |
| `VX_ADDR` | `0.0.0.0:7070` in the image | Listen address |
| `VX_GITHUB_TOKEN` | empty | GitHub API and HTTPS git access |
| `VX_PUBLIC_URL` | empty | This server's public address, like `https://vx.example.com`; needed for the GitHub App (callbacks, webhooks) and the CLI login |
| `VX_SECRET_KEY` | empty | 32 bytes, 64 hex characters or base64 (`openssl rand -hex 32`); seals the GitHub App's credentials and the tokens and webhook secrets of GitLab, Gitea, Forgejo and Bitbucket repositories at rest. Required once a GitHub App exists: the server does not start without it. Keep it with your backups; never change it |
| `VX_SIGNUP` | `invite` | Who may create an account by signing in with GitHub: `invite` (logins a superadmin invited), `open` (anyone) or `closed` (nobody new) |
| `VX_GITHUB_URL`, `VX_GITHUB_API_URL` | github.com | GitHub Enterprise Server only: `https://HOST` and `https://HOST/api/v3` (set both) |
| `VX_GITLAB_URL` | `https://gitlab.com` | GitLab address repositories default to (`--gitlab-url` overrides per repository) |
| `VX_GITLAB_TOKEN` | empty | GitLab token for repositories on `VX_GITLAB_URL` |
| `VX_GITEA_URL`, `VX_FORGEJO_URL` | empty | Gitea or Forgejo address repositories default to; without it every repository needs `--gitea-url` or `--forgejo-url` |
| `VX_GITEA_TOKEN`, `VX_FORGEJO_TOKEN` | empty | Token for repositories on that address |
| `VX_BITBUCKET_TOKEN` | empty | Bitbucket Cloud access token, or `USERNAME:APP_PASSWORD` |
| `VX_GITLAB_WEBHOOK_SECRET`, `VX_GITEA_WEBHOOK_SECRET`, `VX_FORGEJO_WEBHOOK_SECRET`, `VX_BITBUCKET_WEBHOOK_SECRET` | empty | Server-wide webhook secret per host, for repositories without their own |
| `VX_<HOST>_API_URL` | derived | API address when it is not the default (`URL/api/v4`, `URL/api/v1`, `https://api.bitbucket.org/2.0`) |
| `VX_METRICS_TOKEN` | empty | Bearer token for `GET /metrics`; empty: no `/metrics` (see [Metrics](#metrics)) |
| `VX_RATE_API_TOKEN`, `VX_RATE_API_IP`, `VX_RATE_SIGNIN`, `VX_RATE_SIGNUP`, `VX_RATE_CLI_LOGIN`, `VX_RATE_WEBHOOK` | see [Rate limits and quotas](#rate-limits-and-quotas) | Rate limits; `VX_RATE_LIMITS=off` turns all off |
| `VX_QUOTA_REPOS`, `VX_QUOTA_STORED`, `VX_QUOTA_IMPORT`, `VX_QUOTA_PUSH`, `VX_QUOTA_OPEN_EFFORTS` | no limit | Default quotas of every organization |
| `VX_DISK_WARN_PERCENT` | `10` | Warn superadmins below this share of free disk |
| `VX_AUDIT_RETENTION` | forever | Delete operation log entries older than this, like `365d` (at least 7 days) |
| `VX_REQUIRE_DPA`, `VX_DPA_DIR` | off | Ask organizations to accept the data processing agreement, with its versions in that directory (see [Data processing agreement](#data-processing-agreement)) |
| `POSTGRES_PASSWORD` | `vx` | Database password used by compose |
| `VX_VERSION` | `latest` | Image tag |

## Monitoring

- `GET /healthz` returns `ok` when the server can reach Postgres.
- `GET /metrics` serves Prometheus metrics with `VX_METRICS_TOKEN` (see [Metrics](#metrics)).
- The audit log exports as JSONL or CSV: `vx admin audit` and Admin > Audit ([organizations.md](organizations.md#audit-log)).
- Logs go to stdout: `docker compose logs -f vx`.
