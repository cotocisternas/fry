#!/usr/bin/env bash
# Dedicated generated roots only. See tooling/rpc/README.md for integration.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
fail() { echo "proto: $*" >&2; exit 2; }
usage() {
  cat <<'USAGE'
Usage: scripts/generate/proto.sh bootstrap
       scripts/generate/proto.sh identity
       scripts/generate/proto.sh lint INPUT
       scripts/generate/proto.sh generate INPUT OUTPUT
       scripts/generate/proto.sh check INPUT OUTPUT
       scripts/generate/proto.sh fixture [OUTPUT]

INPUT is an explicit local proto tree (including local imports), without symlinks
or registry dependencies. STANDARD lint and descriptor compilation run first.
OUTPUT is a dedicated generated tree containing go/ and ts/. Generate accepts
only a missing, empty, or previously marked generated tree. Check regenerates in
scratch and compares the entire tree without changing OUTPUT. Fixture defaults
to .artifacts/rpc/fixture and also compiles Go, typechecks TS and bundles an IIFE.

Bootstrap installs the mise-pinned native tools and locked generator dependencies.
Other commands require setup to be complete and do not install tools.
Requires Linux, mise, Bash, coreutils, find and diff. Docker is not used.
USAGE
}
[[ $# -gt 0 ]] || { usage; exit 2; }
action=$1; shift
case "$action" in -h|--help|help) usage; exit ;; esac
case "$action" in
  bootstrap|identity) [[ $# == 0 ]] || fail 'Unexpected arguments.' ;;
  lint) [[ $# == 1 ]] || fail 'lint requires INPUT.' ;;
  generate|check) [[ $# == 2 ]] || fail "$action requires INPUT OUTPUT." ;;
  fixture) [[ $# -le 1 ]] || fail 'fixture accepts at most OUTPUT.' ;;
  *) usage >&2; exit 2 ;;
esac
[[ $(uname -s) == Linux ]] || fail 'The reference workflow requires Linux.'
if [[ $action == bootstrap ]]; then
  exec "$repo_root/scripts/setup.sh"
fi
if [[ $action == identity ]]; then
  cd "$repo_root"
  "$repo_root/scripts/toolchain.sh" versions
  "$repo_root/scripts/toolchain.sh" exec protoc-gen-go --version
  "$repo_root/scripts/toolchain.sh" exec protoc-gen-connect-go --version
  exit
fi
if [[ $action == fixture ]]; then
  input="$repo_root/tooling/rpc/fixture/proto"
  output=${1:-$repo_root/.artifacts/rpc/fixture}
else
  input=$1
  output=${2:-}
fi
[[ ! -L $input ]] || fail 'INPUT must not be a symlink.'
input=$(realpath -e -- "$input")
[[ -d $input ]] || fail 'INPUT must be a directory.'
[[ -z $(find "$input" -type l -print -quit) ]] || fail 'INPUT must not contain symlinks.'
[[ $(du -sk "$input" | cut -f1) -le 65536 ]] || fail 'INPUT exceeds the 64 MiB budget.'
[[ -n $(find "$input" -type f -name '*.proto' -print -quit) ]] || fail 'INPUT has no proto files.'
if [[ -n $output ]]; then
  [[ ! -L $output ]] || fail 'OUTPUT must not be a symlink.'
  output=$(realpath -m -- "$output")
  [[ $output != "$input" && $output != "$input/"* && $input != "$output/"* ]] || fail 'INPUT and OUTPUT must not overlap.'
  [[ "$repo_root/.artifacts/rpc" != "$output" && "$repo_root/.artifacts/rpc" != "$output/"* ]] || fail 'OUTPUT must not contain tool scratch space.'
  [[ $output != / && $output != "$repo_root" ]] || fail 'OUTPUT must be a dedicated generated directory.'
  [[ ! -e $output || -d $output ]] || fail 'OUTPUT must be a directory.'
  [[ -z $(find "$output" -type l -print -quit 2>/dev/null || true) ]] || fail 'OUTPUT must not contain symlinks.'
  if [[ -d $output && -n $(find "$output" -mindepth 1 -print -quit) ]]; then
    [[ -f $output/.fry-rpc-generated ]] || fail 'Refusing an unmarked nonempty OUTPUT tree.'
    [[ $(cat "$output/.fry-rpc-generated") == 'FRY local RPC generated tree v1' ]] || fail 'Unrecognized OUTPUT marker.'
  fi
fi
mkdir -p "$repo_root/.artifacts/rpc"
scratch=$(mktemp -d "$repo_root/.artifacts/rpc/run.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
# Stage generation before touching checked-in outputs. Native execution retains
# input/output budgets and a deadline, but is not a container security sandbox.
cd "$repo_root"
timeout --kill-after=10s 180s "$repo_root/scripts/toolchain.sh" exec \
  "$repo_root/tooling/rpc/run.sh" "$action" "$input" "$scratch"
case "$action" in
  lint) echo 'Proto lint and descriptor build passed.' ;;
  check)
    [[ -d $output ]] || fail 'OUTPUT missing; generate and review it first.'
    diff -ru -- "$output" "$scratch"
    echo 'Generated Go/TS tree matches.'
    ;;
  generate|fixture)
    # Checks above establish that replacement cannot delete handwritten files.
    mkdir -p -- "$(dirname -- "$output")"
    if [[ -d $output ]]; then rm -rf -- "$output"; fi
    mv -- "$scratch" "$output"
    echo "Generated tree: $output"
    ;;
esac
