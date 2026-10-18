#!/usr/bin/env bash
# Behavioral checks through the same public wrapper used by Make integration.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
proto="$repo_root/scripts/generate/proto.sh"
mkdir -p "$repo_root/.artifacts/rpc"
work=$(mktemp -d "$repo_root/.artifacts/rpc/verify.XXXXXXXX")
trap 'rm -rf -- "$work"' EXIT
input="$repo_root/tooling/rpc/fixture/proto"
"$proto" lint "$input"
"$proto" generate "$input" "$work/generated"
"$proto" check "$input" "$work/generated"
# A changed file must fail the check and remain changed afterwards.
printf '\n// intentional drift\n' >> "$work/generated/ts/fixture/v1/echo_pb.ts"
before=$(sha256sum "$work/generated/ts/fixture/v1/echo_pb.ts")
if "$proto" check "$input" "$work/generated" > "$work/drift.log" 2>&1; then
  echo 'check accepted drift' >&2; exit 1
fi
[[ $(sha256sum "$work/generated/ts/fixture/v1/echo_pb.ts") == "$before" ]]
# Replacement removes obsolete generated files after successful staging.
printf 'obsolete\n' > "$work/generated/obsolete"
"$proto" generate "$input" "$work/generated"
[[ ! -e $work/generated/obsolete ]]
"$proto" check "$input" "$work/generated"
mkdir "$work/handwritten"
printf 'keep me\n' > "$work/handwritten/source.txt"
if "$proto" generate "$input" "$work/handwritten" > "$work/refusal.log" 2>&1; then
  echo 'generate accepted handwritten output' >&2; exit 1
fi
[[ $(cat "$work/handwritten/source.txt") == 'keep me' ]]
if "$proto" check "$input" "$work/missing" > "$work/missing.log" 2>&1; then
  echo 'check accepted missing output' >&2; exit 1
fi
[[ ! -e $work/missing ]]
# Failed generation must preserve the previous usable output in full.
mkdir "$work/invalid"
printf 'syntax = "proto3"; this is invalid;\n' > "$work/invalid/broken.proto"
cp -R "$work/generated" "$work/saved"
if "$proto" generate "$work/invalid" "$work/generated" > "$work/invalid.log" 2>&1; then
  echo 'generate accepted invalid protobuf input' >&2; exit 1
fi
diff -ru "$work/saved" "$work/generated"
ln -s "$work/generated" "$work/output-link"
if "$proto" generate "$input" "$work/output-link" > "$work/output-link.log" 2>&1; then
  echo 'generate accepted a symlink output root' >&2; exit 1
fi
[[ -L $work/output-link ]]
diff -ru "$work/saved" "$work/generated"
mkdir "$work/input"
ln -s "$input/fixture/v1/echo.proto" "$work/input/escape.proto"
if "$proto" lint "$work/input" > "$work/symlink.log" 2>&1; then
  echo 'lint accepted a symlink' >&2; exit 1
fi
"$proto" fixture "$work/fixture-a"
"$proto" fixture "$work/fixture-b"
diff -ru "$work/fixture-a" "$work/fixture-b"
[[ $(stat -c %u "$work/fixture-a/go/fixture/v1/echo.pb.go") == "$(id -u)" ]]
echo 'Native offline fixture, deterministic generation, drift detection, stale cleanup, ownership and refusal checks passed.'
