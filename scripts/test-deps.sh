#!/usr/bin/env bash
# Native lifecycle tooling lives in its own module, outside production's graph.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
# Resolve the exact mise-managed compiler through the native toolchain. This
# Go-only module does not require generator/npm setup fingerprints to match.
native_go_root=$("$repo_root/scripts/toolchain.sh" go env GOROOT)
export PATH="$native_go_root/bin:$PATH"
module="$repo_root/tooling/testdeps"
export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly GOPROXY=off GOSUMDB=off
go_version=$(awk '$1 == "go" { print $2 }' "$module/go.mod")
[[ $(go env GOVERSION) == "go$go_version" ]] || { echo "Native Go $go_version is required; run scripts/setup.sh." >&2; exit 2; }
action=${1:-help}
case "$action" in
  test)
    shift
    cd "$module"
    exec go test -race -count=1 "$@" ./...
    ;;
  vet)
    shift
    cd "$module"
    exec go "$action" "$@" ./...
    ;;
  lint|fmt-check)
    shift
    cd "$module"
    if [[ "$action" == lint ]]; then
      exec "$repo_root/scripts/toolchain.sh" lint run --config "$repo_root/.golangci.yml" "$@" ./...
    fi
    exec "$repo_root/scripts/toolchain.sh" lint fmt --config "$repo_root/.golangci.yml" --diff "$@" ./...
    ;;
esac
cache="${XDG_CACHE_HOME:-$HOME/.cache}/fry/testdeps/bin"
mkdir -p "$cache"
# The key includes the pinned image references and sources. Go's build cache handles
# compilation; this executable preserves the child's status (go run cannot).
key=$(cd "$module" && find . -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) -print0 | sort -z | xargs -0 sha256sum | sha256sum)
binary="$cache/${key:0:32}"
if [[ ! -x "$binary" ]]; then
  temporary=$(mktemp "$cache/build.XXXXXX")
  trap 'rm -f -- "$temporary"' EXIT
  (cd "$module" && go build -trimpath -o "$temporary" ./cmd/fry-testdeps)
  chmod 755 "$temporary"
  mv -- "$temporary" "$binary"
fi
exec "$binary" "$@"
