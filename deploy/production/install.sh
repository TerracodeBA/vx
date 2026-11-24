#!/usr/bin/env bash
# Turn a fresh Ubuntu 24.04 server into a running vx, as root:
#   curl -fsSLo install.sh https://raw.githubusercontent.com/TerracodeBA/vx/main/deploy/production/install.sh
#   less install.sh          # read it first
#   bash install.sh app.example.com [GIT_REF]
# Point the domain's DNS A record (and AAAA) at this server before running it,
# so Caddy can get a certificate. Safe to run again.
set -euo pipefail
DOMAIN=${1:?usage: install.sh DOMAIN [GIT_REF]}
REF=${2:-main}
SRC=/opt/vx/src
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

echo "== Packages: Docker, git, firewall, automatic security updates, backups"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q ca-certificates git ufw unattended-upgrades docker.io docker-compose-v2 rclone openssl
systemctl enable --now docker unattended-upgrades
if [ ! -f /etc/docker/daemon.json ]; then
  printf '{"log-driver":"json-file","log-opts":{"max-size":"20m","max-file":"5"}}\n' > /etc/docker/daemon.json
  systemctl restart docker
fi

echo "== Firewall: SSH, HTTP, HTTPS only"
ufw allow OpenSSH >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw allow 443/udp >/dev/null
ufw --force enable >/dev/null

echo "== vx source at $REF"
mkdir -p /opt/vx /var/backups/vx
if [ ! -d $SRC/.git ]; then git clone -q https://github.com/TerracodeBA/vx.git $SRC; fi
git -C $SRC fetch -q --tags origin
git -C $SRC checkout -q --detach "$(git -C $SRC rev-parse "origin/$REF" 2>/dev/null || git -C $SRC rev-parse "$REF")"

cd $SRC/deploy/production
ENV=/opt/vx/.env
if [ ! -f $ENV ]; then
  umask 077
  printf 'VX_DOMAIN=%s\nPOSTGRES_PASSWORD=%s\n' "$DOMAIN" "$(openssl rand -hex 24)" > $ENV
fi
# The key that seals secrets at rest (GitHub App credentials). Made once on
# this server and never replaced: secrets stored with it open only with it.
if ! grep -q '^VX_SECRET_KEY=' $ENV; then
  (umask 077; printf 'VX_SECRET_KEY=%s\n' "$(openssl rand -hex 32)" >> $ENV)
fi
sed -i "s/^VX_DOMAIN=.*/VX_DOMAIN=$DOMAIN/" $ENV
ln -sf $ENV .env
export VX_COMMIT=$(git -C $SRC rev-parse --short=12 HEAD) VX_VERSION=$(git -C $SRC describe --tags --always)

echo "== Build and start (first build takes a few minutes)"
docker compose up -d --build
docker image prune -f >/dev/null

echo "== Nightly backup at 03:15"
install -m 755 $SRC/deploy/production/backup.sh /usr/local/sbin/vx-backup
printf 'SHELL=/bin/bash\n15 3 * * * root /usr/local/sbin/vx-backup >> /var/log/vx-backup.log 2>&1\n' > /etc/cron.d/vx-backup

echo "== Waiting for https://$DOMAIN"
for i in $(seq 1 60); do
  if curl -fsS -o /dev/null "https://$DOMAIN/healthz"; then
    echo "vx $VX_VERSION ($VX_COMMIT) is up at https://$DOMAIN"
    echo
    echo "Create the first person (prints their token once):"
    echo "  cd $SRC/deploy/production && docker compose exec vx vx-server user-add HANDLE --name 'Full Name' --email you@example.com"
    exit 0
  fi
  sleep 5
done
echo "https://$DOMAIN did not answer yet. Check DNS, then: cd $SRC/deploy/production && docker compose logs caddy vx" >&2
exit 1
