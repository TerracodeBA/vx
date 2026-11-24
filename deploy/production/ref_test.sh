#!/usr/bin/env bash
# Checks that the deploy scripts' ref lookup resolves branches and tags:
#   bash deploy/production/ref_test.sh
set -euo pipefail
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
git init -q -b main "$T/up" && git -C "$T/up" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m one
git -C "$T/up" tag -a v9.9.9 -m release
git -C "$T/up" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m two
git clone -q "$T/up" "$T/src"
SRC=$T/src
lookup() { local REF=$1; git -C $SRC rev-parse --verify --quiet "origin/$REF^{commit}" || git -C $SRC rev-parse --verify "$REF^{commit}"; }
# The scripts must use exactly this lookup.
for f in "$(dirname "$0")/update.sh" "$(dirname "$0")/install.sh"; do
  grep -q 'rev-parse --verify --quiet "origin/$REF^{commit}" || git -C $SRC rev-parse --verify "$REF^{commit}"' "$f" || { echo "$f does not use the tag-safe lookup" >&2; exit 1; }
done
[ "$(lookup main)" = "$(git -C "$T/up" rev-parse main)" ] || { echo "branch main resolved wrong" >&2; exit 1; }
[ "$(lookup v9.9.9)" = "$(git -C "$T/up" rev-parse 'v9.9.9^{commit}')" ] || { echo "tag v9.9.9 resolved wrong" >&2; exit 1; }
[ "$(lookup v9.9.9 | wc -l)" = 1 ] || { echo "lookup printed more than one line" >&2; exit 1; }
git -C $SRC checkout -q --detach "$(lookup v9.9.9)" && echo "ok: branch and tag resolve"
