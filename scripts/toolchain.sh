#!/usr/bin/env bash
# Native dispatch only. setup.sh is the explicit installation boundary.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$repo_root/tooling/versions.env"
fry_fail() { echo "toolchain: $*" >&2; exit 2; }
command -v mise >/dev/null || fry_fail 'mise is required; install it, then run scripts/setup.sh.'
[[ $(uname -s) == Linux ]] || fry_fail 'Native tooling currently supports Linux.'
case $(uname -m) in x86_64|aarch64|arm64) ;; *) fry_fail 'Native tooling requires amd64 or arm64.' ;; esac
[[ ${FRY_TOOLCHAIN:-native} == native ]] || fry_fail 'Everyday tooling is native. Remove FRY_TOOLCHAIN=docker.'
export FRY_TOOLCHAIN=native GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly
export MISE_AUTO_INSTALL=0 MISE_EXEC_AUTO_INSTALL=0 MISE_NOT_FOUND_AUTO_INSTALL=0 MISE_TASK_RUN_AUTO_INSTALL=0
export MISE_NO_HOOKS=1 MISE_AUTO_ENV=0
# Isolate configuration, retaining the user's existing mise installations.
export MISE_CONFIG_DIR="$repo_root/.artifacts/native-toolchain/config"
export MISE_SYSTEM_CONFIG_DIR="$repo_root/.artifacts/native-toolchain/system-config"
unset MISE_GLOBAL_CONFIG_FILE MISE_CONFIG_FILE MISE_GLOBAL_CONFIG_ROOT MISE_SYSTEM_CONFIG_FILE MISE_ENV BASH_ENV ENV
native_root="$repo_root/.artifacts/native-toolchain/$(uname -m)"
export FRY_RPC_NODE_MODULES="$native_root/npm/node_modules"
export FRY_RPC_BIN="$native_root/bin"

fry_mise() { command mise -C "$repo_root" "$@"; }
fry_backend() {
  case "$1" in
    go) echo core:go ;; node) echo core:node ;;
    buf) echo aqua:bufbuild/buf ;;
    *) fry_fail "Unknown managed tool: $1" ;;
  esac
}
fry_pin() {
  local backend
  case "$1" in
    lint) awk '$1 == "github.com/golangci/golangci-lint/v2" {sub(/^v/, "", $2); print $2}' "$repo_root/tooling/native/golangci-lint/go.mod"; return ;;
    sqlc) awk '$1 == "github.com/sqlc-dev/sqlc" {sub(/^v/, "", $2); print $2}' "$repo_root/tooling/native/sqlc/go.mod"; return ;;
  esac
  backend=$(fry_backend "$1") || return
  fry_mise config get --file "$repo_root/mise.toml" "tools.$backend"
}
fry_args() {
  mise_tools=()
  local tool backend pin
  for tool in "$@"; do
    case "$tool" in lint|sqlc) tool=go ;; esac
    backend=$(fry_backend "$tool") || return
    pin=$(fry_pin "$tool") || return
    [[ -n $pin ]] || fry_fail "Missing pin for $tool in mise.toml."
    [[ " ${mise_tools[*]} " == *" $backend@$pin "* ]] || mise_tools+=("$backend@$pin")
  done
}
fry_binary() {
  local name=$1
  case "$name" in
    lint) echo "$native_root/bin/golangci-lint"; return ;;
    sqlc) echo "$native_root/bin/sqlc"; return ;;
  esac
  fry_mise which --tool "$(fry_backend "$1")@$(fry_pin "$1")" "$name"
}
fry_validate() {
  local tool binary expected actual
  for tool in "$@"; do
    binary=$(fry_binary "$tool")
    [[ -x $binary ]] || fry_fail "$tool is missing; run scripts/setup.sh."
    expected=$(fry_pin "$tool")
    case "$tool" in
      go) actual=$("$binary" env GOVERSION); [[ $actual == "go$expected" ]] || fry_fail "Go mismatch: $actual; expected $expected." ;;
      node) actual=$("$binary" --version); [[ $actual == "v$expected" ]] || fry_fail "Node mismatch: $actual; expected $expected." ;;
      lint)
        fry_validate_native golangci-lint "$binary"
        actual=$("$binary" version)
        [[ $actual == *"version $expected built with go$(fry_pin go) from fry-source on source-build"* ]] || fry_fail "golangci-lint source-build mismatch: $actual"
        ;;
      sqlc)
        fry_validate_native sqlc "$binary"
        actual=$("$binary" version); [[ $actual == "v$expected" ]] || fry_fail "sqlc mismatch: $actual; expected $expected."
        ;;
      buf) actual=$("$binary" --version); [[ $actual == "$expected" ]] || fry_fail "Buf mismatch: $actual; expected $expected." ;;
    esac
  done
}
fry_validate_native() {
  python3 "$repo_root/tooling/native/provenance.py" verify "$repo_root" "$1" \
    "$(fry_binary go)" "$2" "$(fry_pin go)" "$FRY_NATIVE_BUILD_SCHEMA"
}
fry_ensure() {
  fry_args "$@"
  fry_mise install --quiet --locked --dry-run-code "${mise_tools[@]}" || fry_fail 'Pinned tools are missing or unlocked; run scripts/setup.sh.'
  fry_validate "$@"
}
fry_fingerprint() {
  (cd "$repo_root" && sha256sum mise.toml mise.lock tooling/versions.env \
    go.mod go.sum third_party/go-imap/go.mod third_party/go-imap/go.sum \
    third_party/go-smtp/go.mod third_party/go-smtp/go.sum \
    tooling/testdeps/go.mod tooling/testdeps/go.sum \
    tooling/rpc/tools/go.mod tooling/rpc/tools/go.sum tooling/rpc/fixture/go.mod tooling/rpc/fixture/go.sum \
    tooling/rpc/package.json tooling/rpc/package-lock.json scripts/setup.sh scripts/toolchain.sh \
    scripts/doctor.sh tooling/native/build.sh tooling/native/provenance.py tooling/native/*/go.mod tooling/native/*/go.sum)
}
fry_prepared() {
  [[ -f $native_root/inputs.sha256 ]] || fry_fail 'Generator/npm tools are missing; run scripts/setup.sh.'
  cmp -s "$native_root/inputs.sha256" <(fry_fingerprint) || fry_fail 'Generator/npm inputs changed; run scripts/setup.sh.'
  local binary
  for binary in "$FRY_RPC_BIN/protoc-gen-go" "$FRY_RPC_BIN/protoc-gen-connect-go" \
    "$FRY_RPC_NODE_MODULES/.bin/protoc-gen-es" "$FRY_RPC_NODE_MODULES/.bin/tsc" "$FRY_RPC_NODE_MODULES/.bin/esbuild"; do
    [[ -x $binary ]] || fry_fail "Missing $binary; run scripts/setup.sh."
  done
}
# Resolve the root config while preserving the caller's working directory.
fry_exec_at() {
  local directory=$1 managed_path="$FRY_RPC_BIN:$FRY_RPC_NODE_MODULES/.bin" tool name binary
  shift
  for tool in "${mise_tools[@]}"; do
    case ${tool%@*} in
      core:go) name=go ;; core:node) name=node ;;
      aqua:bufbuild/buf) name=buf ;;
      *) fry_fail "Unknown execution tool: $tool" ;;
    esac
    binary=$(fry_mise which --tool "$tool" "$name")
    managed_path+=":$(dirname -- "$binary")"
  done
  # mise can preserve an activated shell's existing PATH order. Put the exact
  # validated installs first after entering its environment, for child tools too.
  # shellcheck disable=SC2016 # Positional parameters expand in the child shell.
  fry_mise exec --no-deps "${mise_tools[@]}" -- bash -c \
    'cd -- "$1"; export PATH="$2:$PATH"; shift 2; exec "$@"' fry-toolchain "$directory" "$managed_path" "$@"
}
fry_main() {
  [[ $# -gt 0 ]] || fry_fail 'Usage: toolchain.sh go|test-go|lint|sqlc|buf|exec|receipts|versions COMMAND...'
  local action=$1 work_dir
  shift
  work_dir=$(pwd -P)
  [[ $work_dir == "$repo_root" || $work_dir == "$repo_root/"* ]] || fry_fail 'Run tools from the FRY checkout.'
  case "$action" in
    go) fry_ensure go; fry_exec_at "$work_dir" "$(fry_binary go)" "$@" ;;
    test-go) fry_ensure go node; fry_exec_at "$work_dir" "$(fry_binary go)" "$@" ;;
    lint) fry_ensure go lint; fry_exec_at "$work_dir" "$(fry_binary lint)" "$@" ;;
    sqlc|buf) fry_ensure "$action"; fry_exec_at "$work_dir" "$(fry_binary "$action")" "$@" ;;
    exec)
      [[ $# -gt 0 ]] || fry_fail 'exec requires a command.'
      fry_ensure go node lint sqlc buf
      fry_prepared
      export PATH="$FRY_RPC_BIN:$FRY_RPC_NODE_MODULES/.bin:$PATH"
      fry_exec_at "$work_dir" "$@"
      ;;
    receipts)
      fry_ensure go node
      fry_exec_at "$work_dir" "$repo_root/scripts/tests/receipts.sh" "$@"
      ;;
    versions)
      fry_ensure go node lint sqlc buf
      echo 'toolchain=native (mise; installation disabled)'
      "$(fry_binary go)" version
      "$(fry_binary node)" --version
      "$(fry_binary lint)" version
      "$(fry_binary sqlc)" version
      "$(fry_binary buf)" --version
      ;;
    test-image) fry_fail 'Compiler tool images were removed. Use the explicit test-dependency image targets.' ;;
    *) fry_fail "Unknown action: $action" ;;
  esac
}
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then fry_main "$@"; fi
