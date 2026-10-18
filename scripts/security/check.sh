#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
# shellcheck disable=SC1091 # Shared dispatcher resolves its own repository root.
source "$repo_root/scripts/toolchain.sh"
[[ $# == 0 ]] || fry_fail 'Usage: scripts/security/check.sh'
fry_ensure go node lint sqlc
export GOENV=off GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly
export CGO_ENABLED=0 GOOS=linux
GOARCH=$("$(fry_binary go)" env GOHOSTARCH)
export GOARCH GOAMD64=v1 GOARM64=v8.0 GOEXPERIMENT='' GOFIPS140=off
# shellcheck disable=SC2154 # native_root is defined by the sourced dispatcher.
export GOMODCACHE="$native_root/go-mod" GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
export GOPRIVATE='' GONOSUMDB='' GONOPROXY='' GOINSECURE=''
fry_exec_at "$repo_root" python3 "$repo_root/tooling/security/check.py"
