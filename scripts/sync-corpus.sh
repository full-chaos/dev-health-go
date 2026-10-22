#!/usr/bin/env bash
# Copies the access-token.v1 golden corpus byte-for-byte from the committed
# origin/main of dev-health-ops and records the source sha and a sha256 per file.
# Usage: scripts/sync-corpus.sh /path/to/dev-health-ops-clone
set -euo pipefail
ops="${1:?ops clone path}"
src=contracts/auth/v1/golden/access-token
dest="$(cd "$(dirname "$0")/.." && pwd)/authverify/testdata/access-token"
git -C "$ops" fetch -q origin main
sha="$(git -C "$ops" rev-parse origin/main)"
rm -rf "$dest"
mkdir -p "$dest"
git -C "$ops" archive "$sha" "$src" | tar -x -C "$dest" --strip-components=5
{
  echo "ops_commit $sha"
  echo "ops_path $src"
  (cd "$dest" && find . -type f | LC_ALL=C sort | sed 's#^\./##' | while read -r f; do echo "$(sha256sum "$f" | cut -d' ' -f1)  $f"; done)
} > "$dest/../SOURCE"
