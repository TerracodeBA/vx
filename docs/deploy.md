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
| `POSTGRES_PASSWORD` | `vx` | Database password used by compose |
| `VX_VERSION` | `latest` | Image tag |

## Monitoring

- `GET /healthz` returns `ok` when the server can reach Postgres.
- Logs go to stdout: `docker compose logs -f vx`.
