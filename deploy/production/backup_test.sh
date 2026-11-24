#!/usr/bin/env bash
# Restore test: backs up a running vx into MinIO with backup.sh, throws the
# server away (database, data volume and local backups), and brings it back
# on a fresh one with restore.sh from the off-site copy alone. Then checks
# that a person can sign in and read a file they landed before the backup.
# Needs Docker and Go; takes about a minute:
#   bash deploy/production/backup_test.sh
# The test stack (Postgres, MinIO, vx-server from this source tree) runs as
# its own compose project on free local ports and is removed at the end.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
SRC=$(cd "$HERE/../.." && pwd)
T=$(mktemp -d)
P=vxbackuptest$$
MINIO_IMAGE=${VX_TEST_MINIO_IMAGE:-cgr.dev/chainguard/minio:latest}
cleanup() {
  (cd "$T" && docker compose -p $P down -v --remove-orphans >/dev/null 2>&1) || true
  rm -rf "$T"
}
trap cleanup EXIT
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
PORT=$(free_port)

echo "== Build vx-server and vx"
mkdir -p "$T/bin" "$T/backups" "$T/home"
(cd "$SRC" && CGO_ENABLED=0 go build -o "$T/bin/" ./cmd/vx-server ./cmd/vx)

KEY=$(openssl rand -hex 12)
SECRET=$(openssl rand -hex 24)
cat > "$T/.env" <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 12)
VX_SECRET_KEY=$(openssl rand -hex 32)
VX_BACKUP_URL=s3://vx-backups/test-server?endpoint=http://minio:9000
VX_BACKUP_ACCESS_KEY_ID=$KEY
VX_BACKUP_SECRET_ACCESS_KEY=$SECRET
VX_BACKUP_KEEP_DAYS=30
EOF
# The production compose file with the image swapped for the binary just
# built, a published port, and MinIO as the off-site bucket.
cat > "$T/docker-compose.yml" <<EOF
services:
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: vx
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
      POSTGRES_DB: vx
    volumes: [pg:/var/lib/postgresql/data]
    healthcheck: {test: ["CMD-SHELL", "pg_isready -U vx"], interval: 1s, timeout: 3s, retries: 60}
  minio:
    image: $MINIO_IMAGE
    command: ["server", "/data"]
    environment:
      MINIO_ROOT_USER: \${VX_BACKUP_ACCESS_KEY_ID}
      MINIO_ROOT_PASSWORD: \${VX_BACKUP_SECRET_ACCESS_KEY}
    tmpfs: ["/data:mode=1777,size=512m"]
  vx:
    image: alpine:3
    entrypoint: ["vx-server"]
    command: ["serve"]
    depends_on: {postgres: {condition: service_healthy}}
    environment:
      VX_DATABASE_URL: postgres://vx:\${POSTGRES_PASSWORD}@postgres:5432/vx?sslmode=disable
      VX_DATA_DIR: /data
      VX_ADDR: 0.0.0.0:7070
      VX_SECRET_KEY: \${VX_SECRET_KEY}
      VX_BLOB_URL: \${VX_BLOB_URL:-}
      VX_BACKUP_URL: \${VX_BACKUP_URL:-}
      VX_BACKUP_ACCESS_KEY_ID: \${VX_BACKUP_ACCESS_KEY_ID:-}
      VX_BACKUP_SECRET_ACCESS_KEY: \${VX_BACKUP_SECRET_ACCESS_KEY:-}
    volumes:
      - data:/data
      - $T/bin/vx-server:/usr/local/bin/vx-server:ro
    ports: ["127.0.0.1:$PORT:7070"]
volumes:
  pg:
  data:
EOF
cd "$T"
export VX_DIR=$T VX_COMPOSE_DIR=$T VX_ENV_FILE=$T/.env VX_BACKUP_DIR=$T/backups COMPOSE_PROJECT_NAME=$P
up() {
  docker compose up -d --wait postgres minio >/dev/null
  docker compose up -d vx >/dev/null
  for i in $(seq 1 60); do
    curl -fs -o /dev/null "http://127.0.0.1:$PORT/healthz" && return 0
    sleep 1
  done
  docker compose logs vx >&2
  echo "vx did not start" >&2
  exit 1
}

echo "== Start a server and land a file"
up
docker compose exec -T vx vx-server backup create-bucket >/dev/null
TOKEN=$(docker compose exec -T vx vx-server user-add alice --name Alice --email alice@example.com | sed -n 's/^Token (shown once): //p')
[ -n "$TOKEN" ] || { echo "user-add printed no token" >&2; exit 1; }
vx() { VX_SERVER=http://127.0.0.1:$PORT VX_TOKEN=$TOKEN VX_CONFIG=$T/home/vx.json HOME=$T/home "$T/bin/vx" "$@"; }
(cd "$T/home" && vx repo create shop >/dev/null && vx effort new "Backups" --repo shop >/dev/null && vx clone shop backups work >/dev/null)
(cd "$T/home/work" && vx new "Add the restore note" >/dev/null && printf 'survives a restore\n' > note.txt && vx land >/dev/null)

echo "== Back up (local and off-site)"
bash "$HERE/backup.sh"
docker compose exec -T vx vx-server backup list | grep -q '^vx-db-' || { echo "no database backup off-site" >&2; exit 1; }
docker compose exec -T vx vx-server backup list | grep -q '^vx-data-' || { echo "no data backup off-site" >&2; exit 1; }

echo "== Lose the server: database, data volume and local backups"
docker compose rm -sfv vx postgres >/dev/null
docker volume rm ${P}_pg ${P}_data >/dev/null
rm -rf "$T/backups" "$T/home"
mkdir -p "$T/backups" "$T/home"

echo "== A fresh server (empty database) restores the off-site backup"
up
VX_RESTORE_YES=1 bash "$HERE/restore.sh" latest

echo "== Check"
vx() { VX_SERVER=http://127.0.0.1:$PORT VX_TOKEN=$TOKEN VX_CONFIG=$T/home/vx.json HOME=$T/home "$T/bin/vx" "$@"; }
(cd "$T/home" && vx clone shop backups again >/dev/null)
got=$(cat "$T/home/again/note.txt")
[ "$got" = "survives a restore" ] || { echo "restored note.txt = '$got'" >&2; exit 1; }
ls "$T/backups" | grep -q '^vx-db-.*\.dump$' || { echo "the backup was not downloaded" >&2; exit 1; }
echo "ok: backup.sh uploaded to MinIO, restore.sh brought back the database and the files on a fresh server"
