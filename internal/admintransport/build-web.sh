#!/usr/bin/env bash
# Reuse the mise-selected native runtime and locked RPC npm dependencies.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
action=${1:-generate}
[[ $action == generate || $action == check ]] || { echo 'Usage: build-web.sh [generate|check]' >&2; exit 2; }
mkdir -p "$repo_root/.artifacts/rpc"
scratch=$(mktemp -d "$repo_root/.artifacts/rpc/web.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
cd "$repo_root"
timeout --kill-after=10s 120s "$repo_root/scripts/toolchain.sh" exec \
 bash -s -- "$repo_root" "$scratch" <<'BUILD'
 set -euo pipefail
 : "${FRY_RPC_NODE_MODULES:?Missing locked npm tools; run make setup-tools}"
 repo_root=$1 scratch=$2
 cd "$scratch"
 mkdir -p internal/admintransport/browser gen/admin
 cp -R "$repo_root/internal/admintransport/browser/." internal/admintransport/browser/
 cp -R "$repo_root/gen/admin/ts" gen/admin/ts
 ln -s "$FRY_RPC_NODE_MODULES" node_modules
 tsc --noEmit --target es2022 --module esnext --moduleResolution bundler --lib es2022,dom --strict --skipLibCheck internal/admintransport/browser/rpc.ts
 esbuild internal/admintransport/browser/rpc.ts --bundle --platform=browser --format=iife --global-name=FRYAdmin --target=es2022 --minify --legal-comments=eof --outfile="$scratch/rpc.js"
BUILD
output="$repo_root/internal/admintransport/web/rpc.js"
if [[ $action == check ]]; then
 diff -u -- "$output" "$scratch/rpc.js"
 echo 'Admin browser bundle matches.'
else
 cp -- "$scratch/rpc.js" "$output"
fi
