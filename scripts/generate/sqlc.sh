#!/usr/bin/env bash
# Generate/check typed PostgreSQL queries with the mise-pinned native tool.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
action=${1:-generate}
if [[ $# -gt 1 || ! "$action" =~ ^(generate|check|vet)$ ]]; then
  echo 'Usage: scripts/generate/sqlc.sh [generate|check|vet]' >&2
  exit 2
fi
modules=(admin maildirectory receiptstore)
work_root=$repo_root
scratch=''
cleanup() { if [[ -n "$scratch" ]]; then rm -rf -- "$scratch"; fi; }
trap cleanup EXIT
if [[ "$action" == check ]]; then
  mkdir -p "$repo_root/.artifacts/sqlc"
  scratch=$(mktemp -d "$repo_root/.artifacts/sqlc/check.XXXXXXXX")
  work_root=$scratch
  cp "$repo_root/sqlc.yaml" "$scratch/"
  cp -R "$repo_root/db" "$scratch/"
fi
command=generate
if [[ "$action" == vet ]]; then command=vet; fi
(cd "$work_root" && "$repo_root/scripts/toolchain.sh" sqlc "$command" -f sqlc.yaml)
if [[ "$action" == check ]]; then
  for module in "${modules[@]}"; do
    diff -ru "$repo_root/internal/$module/dbgen" "$scratch/internal/$module/dbgen"
  done
fi
