#!/usr/bin/env bash
# Exercise Buf's configured boundary without changing application proto sources.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
cp -R "$repo_root/tooling/rpc/fixture/proto" "$scratch/before"
cp -R "$scratch/before" "$scratch/after"
check() {
  "$repo_root/scripts/toolchain.sh" buf breaking "$scratch/after" \
    --against "$scratch/before" --config "$repo_root/tooling/rpc/breaking.yaml" \
    --against-config "$repo_root/tooling/rpc/breaking.yaml"
}
check
# Removing an RPC must be rejected; success would make the gate meaningless.
sed -i '/rpc Echo(/d' "$scratch/after/fixture/v1/echo.proto"
if check > "$scratch/removal.log" 2>&1; then
  echo 'Compatibility check accepted a removed RPC.' >&2
  exit 1
fi
grep -q 'Previously present RPC.*Echo.*deleted' "$scratch/removal.log"
cp "$scratch/before/fixture/v1/echo.proto" "$scratch/after/fixture/v1/echo.proto"
printf '\nmessage AddedRequest { string name = 1; }\n' >> "$scratch/after/fixture/v1/echo.proto"
check
echo 'API compatibility rejects deleted RPCs and accepts additive messages.'
