#!/usr/bin/env bash
# Explicit downloads/builds for native tools, never service startup.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$repo_root/scripts/toolchain.sh"
source "$repo_root/tooling/native/build.sh"
[[ $# == 0 ]] || fry_fail 'Usage: scripts/setup.sh'
cd "$repo_root"
fry_mise trust "$repo_root/mise.toml"
fry_args go node buf
fry_mise install --locked "${mise_tools[@]}"
fry_validate go node buf
mkdir -p "$native_root"
rm -f -- "$native_root/inputs.sha256"
scratch=$(mktemp -d "$native_root/setup.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
# Disposable module copies prevent downloads from rewriting committed checksums.
download_module() {
  local module=$1 copy="$scratch/modules/${1//\//_}"
  [[ $module != . ]] || copy="$scratch/modules/root"
  mkdir -p "$copy"
  cp "$repo_root/$module/go.mod" "$copy/go.mod"
  cp "$repo_root/$module/go.sum" "$copy/go.sum"
  if [[ $module == . ]]; then ln -s "$repo_root/third_party" "$copy/third_party"; fi
  fry_exec_at "$copy" go mod download
  fry_exec_at "$copy" go mod verify
  cmp -s "$repo_root/$module/go.mod" "$copy/go.mod" || fry_fail "$module/go.mod needs an explicit dependency update."
  cmp -s "$repo_root/$module/go.sum" "$copy/go.sum" || fry_fail "$module/go.sum needs an explicit dependency update."
}
for module in . third_party/go-imap third_party/go-smtp tooling/rpc/tools tooling/rpc/fixture; do download_module "$module"; done
if [[ -f $repo_root/tooling/testdeps/go.mod ]]; then download_module tooling/testdeps; fi
mkdir -p "$scratch/bin" "$scratch/npm"
fry_build_native "$scratch/bin"
fry_exec_at "$repo_root/tooling/rpc/tools" go build -trimpath -buildvcs=false -o "$scratch/bin/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
fry_exec_at "$repo_root/tooling/rpc/tools" go build -trimpath -buildvcs=false -o "$scratch/bin/protoc-gen-connect-go" connectrpc.com/connect/v2/cmd/protoc-gen-connect-go
cp "$repo_root/tooling/rpc/"{package.json,package-lock.json} "$scratch/npm/"
fry_exec_at "$scratch/npm" npm ci --include=dev --include=optional --no-audit --no-fund
cmp -s "$repo_root/tooling/rpc/package-lock.json" "$scratch/npm/package-lock.json" || fry_fail 'npm changed its lockfile.'
rm -rf -- "${native_root:?}/bin" "$native_root/npm"
mv -- "$scratch/bin" "$native_root/bin"
mv -- "$scratch/npm" "$native_root/npm"
fry_fingerprint > "$native_root/inputs.sha256"
"$repo_root/scripts/doctor.sh"
