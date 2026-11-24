# Deploying for a team

One vx server that a team shares. Everyone signs in with their own token, and each person's agents get their own tokens. Read [organizations.md](organizations.md) first for the current limits.

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
docker compose exec vx vx-server user-add alice --name "Alice Example" --email alice@example.com
docker compose exec vx vx-server token-create alice      # another token for an existing user
```

Hand each token to its person privately. People then create their agents themselves:

```sh
vx login --server https://vx.example.com --token <token>
vx agent add backend --env-file ~/.vx-agents/backend.env
```

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
