---
name: deploy
description: "Deploy vx on a server for a team - Docker Compose with Postgres, HTTPS through a reverse proxy, users and agent tokens, GitHub access, backups and upgrades. Use when the user wants to host vx for several people, on a VM or a company server."
when_to_use: "deploy vx for my team; host vx; vx on a server; vx with https; back up vx; upgrade vx"
---

# Deploying vx for a team

This sets up one vx server that a team shares: everyone signs in with their own token, and each person's agents get their own tokens. Read docs/organizations.md in the vx repository for what is and is not ready for larger organizations, and tell the user about the current limits before they invite people (see "Current limits" below).

Ask the user for: the server (SSH host), the domain name pointing at it, and whether vx should open pull requests itself (needs a GitHub token). Never ask them to paste secrets into the chat.

## 1. Server

Any Linux VM with 2 vCPU, 4 GB RAM and disk for the repositories (objects are stored once, compressed). Install Docker with the compose plugin. Open only ports 80 and 443.

## 2. vx and Postgres

```sh
git clone https://github.com/TerracodeBA/vx /opt/vx && cd /opt/vx/deploy
umask 077 && cat > .env <<'EOF'
POSTGRES_PASSWORD=<long random value>
VX_VERSION=latest
# VX_GITHUB_TOKEN=<fine-grained token: Contents + Pull requests read/write>
EOF
docker compose up -d
docker compose exec vx wget -qO- http://127.0.0.1:7070/healthz
```

Generate the password on the server (`openssl rand -base64 32`) and write it into `.env` without echoing it to the chat. The user adds the GitHub token to `.env` themselves.

The compose file binds vx to 127.0.0.1:7070, so it is reachable only through the proxy.

## 3. HTTPS

Use Caddy for automatic certificates. On the host:

```
# /etc/caddy/Caddyfile
vx.example.com {
    reverse_proxy 127.0.0.1:7070
}
```

`systemctl reload caddy`, then open https://vx.example.com and check the sign-in page loads. The vx CLI and API use HTTP/2 over the same address.

## 4. People and agents

Ask the user for the team's organization handle (lowercase, e.g. `acme`). The first person is a superadmin and creates the organization as its owner; everyone else joins it as a member:

```sh
docker compose exec vx vx-server user-add FIRST --admin --org ORG --name "Full Name" --email EMAIL
docker compose exec vx vx-server user-add HANDLE --org ORG --name "Full Name" --email EMAIL
```

Members get the organization's default role, `read`, on its repositories. Owners and admins change roles with `vx org member ORG PERSON --role admin`, `vx org set-default ORG write`, teams (`vx team create ORG/TEAM`) and grants (`vx repo grant REPO team:ORG/TEAM --role write`). Every person also has a personal organization named like them.

Send each token to its person through a private channel (not the chat with you). Each person then runs `vx login --server https://vx.example.com --token ...` and creates tokens for their own agents with `vx agent add NAME`.

## 5. Backups

Back up both Postgres and the data volume (objects and git mirrors), together:

```sh
docker compose exec -T postgres pg_dump -U vx vx | gzip > /backup/vx-$(date +%F).sql.gz
docker run --rm -v deploy_data:/data -v /backup:/backup alpine tar -czf /backup/vx-data-$(date +%F).tgz -C /data .
```

Schedule it daily (cron or a systemd timer) and test a restore once.

## 6. Upgrades

```sh
cd /opt/vx && git pull && cd deploy
docker compose pull && docker compose up -d
```

Migrations run automatically when vx-server starts. Back up first.

Upgrading from v0.1 (no organizations) to v0.2 keeps everyone's access: every existing person becomes an owner of the organizations made from repository owner names and a site admin (a superadmin since v0.5, without access to repositories). Tell the user to prune afterwards as described in docs/deploy.md, "Upgrading to v0.2", and to read "Upgrading to v0.5: superadmins" (`vx org list --all`, `vx org remove`, `vx org set-default ORG none`).

## Current limits (tell the user)

- People see only repositories they have a role on (organization membership, team or direct grant). Superadmins run the server and see no repository without a role; a repository's admins can give them time-limited support access (`vx repo support REPO HANDLE --for 24h`). Keep superadmins few.
- Sign-in is by token. There is no single sign-on yet.
- File contents are stored on the server's disk, so one server instance; scale up, not out.
- Agents are not yet limited to paths, and there are no per-agent budgets.

These are on the roadmap in docs/organizations.md.
