#!/usr/bin/env bash
# Copies the auth golden corpora byte-for-byte from the committed origin/main of
# dev-health-ops and records the source sha and a sha256 per file.
# Usage: scripts/sync-corpus.sh /path/to/dev-health-ops-clone
set -euo pipefail
ops="${1:?ops clone path}"
root="$(cd "$(dirname "$0")/.." && pwd)/authverify/testdata"
git -C "$ops" fetch -q origin main
sha="$(git -C "$ops" rev-parse origin/main)"
sets=("access-token:contracts/auth/v1/golden/access-token:5" "discovery:contracts/auth/v1/examples/discovery:5" "jwks:contracts/auth/v1/examples/jwks:5")
{
  echo "ops_commit $sha"
  for set in "${sets[@]}"; do echo "ops_path ${set#*:}" | sed 's/:[0-9]*$//'; done
} > "$root/SOURCE.head"
for set in "${sets[@]}"; do
  name="${set%%:*}"; rest="${set#*:}"; src="${rest%%:*}"; strip="${rest##*:}"
  rm -rf "${root:?}/$name"
  mkdir -p "$root/$name"
  git -C "$ops" archive "$sha" "$src" | tar -x -C "$root/$name" --strip-components="$strip"
done
{
  cat "$root/SOURCE.head"
  (cd "$root" && find access-token discovery jwks -type f | LC_ALL=C sort | while read -r f; do echo "$(sha256sum "$f" | cut -d' ' -f1)  $f"; done)
} > "$root/SOURCE"
rm "$root/SOURCE.head"
