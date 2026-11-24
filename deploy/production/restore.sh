#!/usr/bin/env bash
# Restore a backup made by backup.sh into this server, as root. Typically on
# a fresh server set up with install.sh, after putting the old server's
# VX_SECRET_KEY (and VX_BACKUP_*, VX_BLOB_* settings) into /opt/vx/.env:
#   bash restore.sh                    # list the backups, local and off-site
#   bash restore.sh latest             # restore the newest one
#   bash restore.sh 2026-11-24-0315    # restore one, by its date stamp
# Backups missing from /var/backups/vx are downloaded from VX_BACKUP_URL and
# checked against their .sha256. The database (and the data volume, when the
# backup has an archive of it) is replaced; the current database is first
# saved as vx-before-restore-*.dump. Asks before it starts unless
# VX_RESTORE_YES=1.
set -euo pipefail
umask 077
VX_DIR=${VX_DIR:-/opt/vx}
ENV=${VX_ENV_FILE:-$VX_DIR/.env}
OUT=${VX_BACKUP_DIR:-/var/backups/vx}
cd "${VX_COMPOSE_DIR:-$VX_DIR/src/deploy/production}"
WANT=${1:-}

setting() {
  [ -f "$ENV" ] || return 0
  sed -n "s/^$1=//p" "$ENV" | tail -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}
# vx-server in a one-off container: works whether or not vx is running.
vxs() { docker compose run --rm --no-deps -T vx "$@"; }

mkdir -p "$OUT" && chmod 700 "$OUT"
docker compose up -d --wait postgres >/dev/null

stamps() {
  {
    ls -1 "$OUT" 2>/dev/null | sed -n 's/^vx-db-\(.*\)\.dump$/\1/p'
    if [ -n "$(setting VX_BACKUP_URL)" ]; then
      vxs backup list | cut -f1 | sed -n 's/^vx-db-\(.*\)\.dump$/\1/p'
    fi
  } | sort -u
}

if [ -z "$WANT" ]; then
  where="local in $OUT"
  [ -z "$(setting VX_BACKUP_URL)" ] || where="$where, off-site in $(setting VX_BACKUP_URL | sed 's/?.*//')"
  echo "Backups ($where):"
  stamps | sed 's/^/  /'
  echo "Restore one with: bash $0 STAMP   (or latest)"
  exit 0
fi
STAMP=$WANT
if [ "$WANT" = latest ]; then
  STAMP=$(stamps | tail -n 1)
  [ -n "$STAMP" ] || { echo "no backups in $OUT or off-site" >&2; exit 1; }
fi

# fetch NAME: make sure $OUT/NAME exists, downloading it if needed.
# Returns 1 when it exists nowhere.
fetch() {
  [ -f "$OUT/$1" ] && return 0
  [ -n "$(setting VX_BACKUP_URL)" ] || return 1
  vxs backup list | cut -f1 | grep -qx "$1" || return 1
  echo "Downloading $1 from $(setting VX_BACKUP_URL | sed 's/?.*//')"
  vxs backup get "$1" > "$OUT/$1.part"
  mv "$OUT/$1.part" "$OUT/$1"
}
DB=vx-db-$STAMP.dump
DATA=vx-data-$STAMP.tgz
fetch "$DB" || { echo "no backup $DB here or off-site; run bash $0 to list them" >&2; exit 1; }
HAVE_DATA=no
if fetch "$DATA"; then HAVE_DATA=yes; fi

if [ -z "$(setting VX_SECRET_KEY)" ]; then
  echo "warning: $ENV has no VX_SECRET_KEY. Secrets of the old server (GitHub App, host tokens) open only with its key." >&2
fi
if [ "$HAVE_DATA" = no ] && [ -z "$(setting VX_BLOB_URL)" ]; then
  echo "warning: the backup has no data archive and VX_BLOB_URL is not set: file contents will be missing." >&2
  echo "  Set VX_BLOB_URL to the bucket the old server used, or restore a backup with a vx-data archive." >&2
fi
if [ "$HAVE_DATA" = yes ] && [ -n "$(setting VX_BLOB_URL)" ]; then
  echo "note: this backup keeps objects on the data volume, but this server reads them from VX_BLOB_URL." >&2
  echo "  After the restore, copy them over: docker compose exec vx vx-server blobs copy --from file:///data/objects --to \"\$VX_BLOB_URL\"" >&2
fi
what="the database"
[ "$HAVE_DATA" = no ] || what="the database and the data volume"
echo "This replaces $what of this server with backup $STAMP."
if [ "${VX_RESTORE_YES:-}" != 1 ]; then
  read -r -p "Type yes to go on: " ok
  [ "$ok" = yes ] || { echo "Nothing changed."; exit 1; }
fi

NOW=$(date +%F-%H%M%S)
docker compose exec -T postgres pg_dump -U vx --format=custom vx > "$OUT/vx-before-restore-$NOW.dump" 2>/dev/null ||
  rm -f "$OUT/vx-before-restore-$NOW.dump"
docker compose stop vx
docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U vx -d postgres \
  -c 'drop database if exists vx with (force)' -c 'create database vx owner vx'
docker compose exec -T postgres pg_restore -U vx -d vx --no-owner --exit-on-error < "$OUT/$DB"
if [ "$HAVE_DATA" = yes ]; then
  docker compose run --rm --no-deps -T --entrypoint sh vx -c 'find /data -mindepth 1 -delete && tar -xzf - -C /data' < "$OUT/$DATA"
fi
docker compose up -d vx
for i in $(seq 1 60); do
  if docker compose exec -T vx wget -qO- http://127.0.0.1:7070/healthz >/dev/null 2>&1; then
    counts=$(docker compose exec -T postgres psql -U vx -d vx -At -c \
      "select (select count(*) from repos) || ' repositories, ' || (select count(*) from efforts) || ' efforts, ' || (select count(*) from changesets) || ' changes'")
    echo "Restored backup $STAMP: $counts. vx is up; the git mirrors fill again on first use."
    exit 0
  fi
  sleep 2
done
echo "vx did not become healthy after the restore: docker compose logs vx" >&2
exit 1
