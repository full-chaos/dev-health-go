#!/usr/bin/env bash
# Copies the auth golden corpora byte-for-byte from the committed origin/main of
# dev-health-ops and records the source sha and a sha256 per file:
#   authverify/testdata/SOURCE                  access-token, discovery and jwks
#   authverify/testdata/workload-token.SOURCE   workload-token
# It also writes the same ops commit into the two pin constants the corpus pin
# tests compare SOURCE against (corpusOpsCommit in accesstoken_test.go,
# workloadCorpusOpsCommit in workload_token_test.go), so one run moves every pin.
# Both constants are checked before anything is written; a missing one stops the
# script with the corpus untouched.
# Usage: scripts/sync-corpus.sh /path/to/dev-health-ops-clone
set -euo pipefail
ops="${1:?ops clone path}"
repo="$(cd "$(dirname "$0")/.." && pwd)"
root="$repo/authverify/testdata"
constants=("authverify/accesstoken_test.go:corpusOpsCommit" "authverify/workload_token_test.go:workloadCorpusOpsCommit")
for entry in "${constants[@]}"; do
  file="${entry%%:*}"; name="${entry##*:}"
  grep -Eq "^const ${name} = \"[0-9a-f]{40}\"\$" "$repo/$file" || {
    echo "sync-corpus: $file has no line 'const $name = \"<40 hex>\"'; nothing was changed" >&2
    exit 1
  }
done
git -C "$ops" fetch -q origin main
sha="$(git -C "$ops" rev-parse origin/main)"

# sync_group <SOURCE file name> <name:ops path:strip>...
sync_group() {
  local out="$1"; shift
  local names=() set name rest src strip
  {
    echo "ops_commit $sha"
    for set in "$@"; do echo "ops_path ${set#*:}" | sed 's/:[0-9]*$//'; done
  } > "$root/$out.head"
  for set in "$@"; do
    name="${set%%:*}"; rest="${set#*:}"; src="${rest%%:*}"; strip="${rest##*:}"
    names+=("$name")
    rm -rf "${root:?}/$name"
    mkdir -p "$root/$name"
    git -C "$ops" archive "$sha" "$src" | tar -x -C "$root/$name" --strip-components="$strip"
  done
  {
    cat "$root/$out.head"
    (cd "$root" && find "${names[@]}" -type f | LC_ALL=C sort | while read -r f; do echo "$(sha256sum "$f" | cut -d' ' -f1)  $f"; done)
  } > "$root/$out"
  rm "$root/$out.head"
}

sync_group SOURCE "access-token:contracts/auth/v1/golden/access-token:5" "discovery:contracts/auth/v1/examples/discovery:5" "jwks:contracts/auth/v1/examples/jwks:5"
sync_group workload-token.SOURCE "workload-token:contracts/auth/v1/golden/workload-token:5"

for entry in "${constants[@]}"; do
  file="${entry%%:*}"; name="${entry##*:}"
  sed -i -E "s/^(const ${name} = \")[0-9a-f]{40}(\")\$/\1${sha}\2/" "$repo/$file"
done
