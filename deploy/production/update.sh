#!/usr/bin/env bash
# Update vx to a git ref (default main), after taking a backup:
#   bash /opt/vx/src/deploy/production/update.sh [GIT_REF]
set -euo pipefail
REF=${1:-main}
SRC=/opt/vx/src
/usr/local/sbin/vx-backup
git -C $SRC fetch -q --tags origin
git -C $SRC checkout -q --detach "$(git -C $SRC rev-parse "origin/$REF" 2>/dev/null || git -C $SRC rev-parse "$REF")"
cd $SRC/deploy/production
export VX_COMMIT=$(git -C $SRC rev-parse --short=12 HEAD) VX_VERSION=$(git -C $SRC describe --tags --always)
docker compose up -d --build
docker image prune -f >/dev/null
echo "vx is now $VX_VERSION ($VX_COMMIT)"
