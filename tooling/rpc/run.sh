#!/usr/bin/env bash
set -euo pipefail
[[ $# == 3 ]] || { echo 'Usage: run.sh ACTION INPUT OUTPUT' >&2; exit 2; }
action=$1 input=$2 output=$3
tool_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
: "${FRY_RPC_NODE_MODULES:?Run through scripts/toolchain.sh exec after make setup-tools}"
work=$(mktemp -d "$(dirname -- "$output")/work.XXXXXXXX")
trap 'rm -rf -- "$work"' EXIT
# Offline dependency resolution after explicit setup; no registry plugins.
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly
ulimit -f 65536
cd "$work"
mkdir proto generated
cp -R "$input/." proto/
cat > buf.yaml <<'CONFIG'
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
CONFIG
cat > buf.gen.yaml <<'CONFIG'
version: v2
plugins:
  - local: protoc-gen-go
    out: generated/go
    opt: [paths=source_relative]
  - local: protoc-gen-connect-go
    out: generated/go
    opt: [paths=source_relative]
  - local: protoc-gen-es
    out: generated/ts
    opt: [target=ts, import_extension=js]
CONFIG
buf lint
buf build --as-file-descriptor-set --output "$work/schema.binpb"
if [[ $action == lint ]]; then exit; fi
buf generate
printf 'FRY local RPC generated tree v1\n' > generated/.fry-rpc-generated
if [[ $action == fixture ]]; then
  mkdir fixture
  cp "$tool_dir"/fixture/{go.mod,go.sum,client.ts,tsconfig.json} fixture/
  cp -R generated/go fixture/gen
  (cd fixture && go test ./...)
  rm -r fixture/gen
  cp -R generated/ts fixture/gen
  ln -s "$FRY_RPC_NODE_MODULES" fixture/node_modules
  (cd fixture && tsc --project tsconfig.json && esbuild client.ts --bundle --platform=browser --format=iife --target=es2022 --minify --outfile="$work/generated/fixture.js")
fi
# Bound generated output separately from the native compiler cache.
[[ $(du -sk generated | cut -f1) -le 131072 ]]
[[ $(find generated -type f | wc -l) -le 10000 ]]
cp -R generated/. "$output/"
