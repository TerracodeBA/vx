#!/usr/bin/env bash
# Back up vx: the Postgres database, and the data volume while objects live
# on it (no VX_BLOB_URL). Keeps 14 days locally in /var/backups/vx.
#
# Off-site: with these in /opt/vx/.env (then `docker compose up -d` once in
# this folder, so the vx container sees them), every backup is also uploaded
# to an S3-compatible bucket (AWS S3, Cloudflare R2, MinIO, Google Cloud
# Storage with HMAC keys), with a .sha256 next to it:
#   VX_BACKUP_URL=s3://BUCKET/PREFIX?region=auto&endpoint=https://ACCOUNT.r2.cloudflarestorage.com
#   VX_BACKUP_ACCESS_KEY_ID=...
#   VX_BACKUP_SECRET_ACCESS_KEY=...
#   VX_BACKUP_KEEP_DAYS=30   (optional; 0 leaves deleting to a bucket lifecycle rule)
# An rclone remote named "offsite" (the v0.4 way) still gets a copy too.
# restore.sh in this folder brings a backup back.
set -euo pipefail
umask 077
VX_DIR=${VX_DIR:-/opt/vx}
ENV=${VX_ENV_FILE:-$VX_DIR/.env}
OUT=${VX_BACKUP_DIR:-/var/backups/vx}
cd "${VX_COMPOSE_DIR:-$VX_DIR/src/deploy/production}"

# setting KEY prints KEY's value from .env (the last one, unquoted).
setting() {
  [ -f "$ENV" ] || return 0
  sed -n "s/^$1=//p" "$ENV" | tail -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}

STAMP=$(date +%F-%H%M)
mkdir -p "$OUT" && chmod 700 "$OUT"
made=()
# Written to .part files and renamed when complete, so a failed run never
# leaves a backup that looks whole.
docker compose exec -T postgres pg_dump -U vx --format=custom vx > "$OUT/vx-db-$STAMP.dump.part"
mv "$OUT/vx-db-$STAMP.dump.part" "$OUT/vx-db-$STAMP.dump"
made+=("vx-db-$STAMP.dump")
if [ -z "$(setting VX_BLOB_URL)" ]; then
  # Objects are on the data volume: back it up with the database.
  docker compose exec -T vx tar -czf - -C /data . > "$OUT/vx-data-$STAMP.tgz.part"
  mv "$OUT/vx-data-$STAMP.tgz.part" "$OUT/vx-data-$STAMP.tgz"
  made+=("vx-data-$STAMP.tgz")
fi
# With VX_BLOB_URL the volume holds only git mirrors, which vx fetches again
# from the git hosts; the bucket's objects are immutable (turn on versioning
# or a replica bucket at the provider, or copy them with vx-server blobs copy).

find "$OUT" -name 'vx-*' -mtime +14 -delete
find "$OUT" -name '*.part' -mmin +600 -delete
# Updates take a backup too: keep at most the 20 newest of each kind.
for kind in db data; do ls -1t "$OUT"/vx-$kind-* 2>/dev/null | tail -n +21 | xargs -r rm -f --; done

offsite=""
if [ -n "$(setting VX_BACKUP_URL)" ]; then
  if ! docker compose exec -T vx sh -c 'test -n "$VX_BACKUP_URL"'; then
    echo "VX_BACKUP_URL is in $ENV but not in the running vx container: run 'docker compose up -d' in $PWD, then back up again" >&2
    exit 1
  fi
  for name in "${made[@]}"; do
    docker compose exec -T vx vx-server backup put "$name" < "$OUT/$name"
  done
  keep=$(setting VX_BACKUP_KEEP_DAYS)
  if [ "${keep:-30}" != 0 ]; then
    docker compose exec -T vx vx-server backup prune --keep-days "${keep:-30}" ||
      echo "warning: could not delete old off-site backups (fine when the key may not delete and a lifecycle rule does it)" >&2
  fi
  offsite=", off-site in $(setting VX_BACKUP_URL | sed 's/?.*//')"
fi
if command -v rclone >/dev/null && rclone listremotes 2>/dev/null | grep -q '^offsite:'; then
  rclone copy "$OUT" offsite:vx-backups --max-age 2d
  offsite="$offsite, rclone offsite:vx-backups"
fi
echo "$(date -Is) backup ok: ${made[*]} ($(du -sh "$OUT" | cut -f1) in $OUT$offsite)"
