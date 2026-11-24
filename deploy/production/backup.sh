#!/usr/bin/env bash
# Back up Postgres and vx's data volume together; keeps 14 days locally in
# /var/backups/vx. With an rclone remote named "offsite" configured
# (for example Cloudflare R2: rclone config), it also copies them there.
set -euo pipefail
umask 077
cd /opt/vx/src/deploy/production
OUT=/var/backups/vx
STAMP=$(date +%F-%H%M)
mkdir -p $OUT && chmod 700 $OUT
docker compose exec -T postgres pg_dump -U vx --format=custom vx > $OUT/vx-db-$STAMP.dump
docker compose exec -T vx tar -czf - -C /data . > $OUT/vx-data-$STAMP.tgz
find $OUT -name 'vx-*' -mtime +14 -delete
# Updates take a backup too: keep at most the 20 newest of each kind.
for kind in db data; do ls -1t $OUT/vx-$kind-* 2>/dev/null | tail -n +21 | xargs -r rm -f --; done
if rclone listremotes 2>/dev/null | grep -q '^offsite:'; then
  rclone copy $OUT offsite:vx-backups --max-age 2d
fi
echo "$(date -Is) backup ok: $(du -sh $OUT | cut -f1) in $OUT"
