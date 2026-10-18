#!/usr/bin/env bash
# Compare administration API source and wire compatibility, using local Git only.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$repo_root"
[[ $# == 0 ]] || { echo 'compatibility.sh accepts no arguments.' >&2; exit 2; }
baseline=$(cat tooling/rpc/admin-baseline.txt)
refs=("$baseline")
if [[ -n ${FRY_PROTO_BASE_REF:-} && $FRY_PROTO_BASE_REF != "$baseline" ]]; then
  refs+=("$FRY_PROTO_BASE_REF")
fi
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
for ref in "${refs[@]}"; do
  [[ $ref =~ ^[0-9a-f]{40}$ ]] || { echo 'API comparison requires a full commit SHA.' >&2; exit 2; }
  git cat-file -e "$ref^{commit}"
  mkdir "$scratch/$ref"
  git archive "$ref" proto/admin | tar -x -C "$scratch/$ref"
  ./scripts/toolchain.sh buf breaking proto/admin \
    --config tooling/rpc/breaking.yaml \
    --against "$scratch/$ref/proto/admin" \
    --against-config tooling/rpc/breaking.yaml
done
echo 'Administration API source/wire compatibility passed.'
