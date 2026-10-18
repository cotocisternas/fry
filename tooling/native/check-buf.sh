#!/usr/bin/env bash
# Check the affected subcomponent in the exact installed executables.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
source "$repo_root/scripts/toolchain.sh"
fry_ensure go buf
buf_directory=$(dirname -- "$(fry_binary buf)")
fry_exec_at "$repo_root" go run "$repo_root/tooling/native/inspect-openpgp.go" \
  "$buf_directory/buf" "$buf_directory/protoc-gen-buf-breaking" "$buf_directory/protoc-gen-buf-lint"
