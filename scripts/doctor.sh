#!/usr/bin/env bash
# Read-only readiness checks; never install tools or start services.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$repo_root/scripts/toolchain.sh"
[[ $# == 0 ]] || fry_fail 'Usage: scripts/doctor.sh'
for host_command in bash make python3 openssl sha256sum cmp diff find sort realpath timeout; do
  command -v "$host_command" >/dev/null || fry_fail "Host prerequisite missing: $host_command"
done
read -r -a host_compiler <<< "${CC:-cc}"
command -v "${host_compiler[0]}" >/dev/null || fry_fail "C compiler missing: ${host_compiler[0]} (needed by race/cgo tests)."
fry_ensure go node lint sqlc buf
fry_prepared
export PATH="$FRY_RPC_BIN:$FRY_RPC_NODE_MODULES/.bin:$PATH"
fry_exec_at "$repo_root" node "$repo_root/tooling/rpc/check-packages.cjs" \
  "$repo_root/tooling/rpc/package-lock.json" "$FRY_RPC_NODE_MODULES"
go_version=$(awk '$1 == "google.golang.org/protobuf" {print $2}' "$repo_root/tooling/rpc/tools/go.mod")
connect_version=$(awk '$1 == "connectrpc.com/connect/v2" {print $2}' "$repo_root/tooling/rpc/tools/go.mod")
[[ $("$FRY_RPC_BIN/protoc-gen-go" --version) == "protoc-gen-go $go_version" ]] || fry_fail 'Go protobuf generator mismatch.'
actual=$("$FRY_RPC_BIN/protoc-gen-connect-go" --version)
[[ $actual == "$connect_version" || $actual == "${connect_version#v}" ]] || fry_fail "Go Connect generator mismatch: $actual"
fry_exec_at "$repo_root" esbuild --version
fry_exec_at "$repo_root" tsc --version
"$repo_root/scripts/toolchain.sh" versions
echo 'Native tools and locked generator/npm dependencies are ready.'
