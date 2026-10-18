#!/usr/bin/env bash
# Explicit, authenticated source builds; ordinary security checks never install.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
# shellcheck disable=SC1091 # Shared dispatcher resolves its own repository root.
source "$repo_root/scripts/toolchain.sh"
[[ $# == 0 ]] || fry_fail 'Usage: scripts/security/setup.sh'
fry_ensure go node
security_root="$repo_root/.artifacts/security-tools/$(uname -m)"
mkdir -p "$security_root"
scratch=$(mktemp -d "$security_root/setup.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
mkdir -p "$scratch/module" "$scratch/bin"
cp "$repo_root/tooling/security/scanners/"{go.mod,go.sum} "$scratch/module/"
export GOENV=off CGO_ENABLED=0 GOOS=linux
GOARCH=$("$(fry_binary go)" env GOHOSTARCH)
export GOARCH GOAMD64=v1 GOARM64=v8.0 GOEXPERIMENT='' GOFIPS140=off
# shellcheck disable=SC2154 # native_root is defined by the sourced dispatcher.
export GOMODCACHE="$native_root/go-mod" GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
export GOPRIVATE='' GONOSUMDB='' GONOPROXY='' GOINSECURE=''
fry_exec_at "$scratch/module" go mod download
fry_exec_at "$scratch/module" go mod verify
fry_exec_at "$scratch/module" go build -trimpath -buildvcs=false -ldflags '-s -w' \
  -o "$scratch/bin/govulncheck" golang.org/x/vuln/cmd/govulncheck
fry_exec_at "$scratch/module" go build -trimpath -buildvcs=false \
  -ldflags '-s -w -X github.com/zricethezav/gitleaks/v8/version.Version=8.30.1' \
  -o "$scratch/bin/gitleaks" github.com/zricethezav/gitleaks/v8
fry_exec_at "$scratch/module" go build -trimpath -buildvcs=false -ldflags '-s -w' \
  -o "$scratch/bin/actionlint" github.com/rhysd/actionlint/cmd/actionlint
cmp -s "$scratch/module/go.mod" "$repo_root/tooling/security/scanners/go.mod" || fry_fail 'Security go.mod changed during setup.'
cmp -s "$scratch/module/go.sum" "$repo_root/tooling/security/scanners/go.sum" || fry_fail 'Security go.sum changed during setup.'
python3 "$repo_root/tooling/security/provenance.py" record "$repo_root" "$(fry_binary go)" "$scratch/bin"
rm -rf -- "${security_root:?}/bin"
mv -- "$scratch/bin" "$security_root/bin"
echo 'Security scanners prepared. Run scripts/security/check.sh.'
