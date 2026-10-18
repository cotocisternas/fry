#!/usr/bin/env bash
# Actual coordinator + member executables, temporary PostgreSQL and synthetic mail.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
source "$repo_root/scripts/lib/testdeps.sh"
fry_native_run "$@"
fry_testdeps_run "$@"
scratch=$(mktemp -d "$repo_root/.fry-replica-smoke.XXXXXX")
container=$FRY_TEST_POSTGRES_CONTAINER
cleanup() {
  rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export FRY_DATABASE_URL="$FRY_TEST_DATABASE_URL"
export FRY_LOCAL_IMAP_PASSWORD='disposable-replica-smoke'
"$repo_root/scripts/toolchain.sh" go build -o "$scratch/fry" ./cmd/fry
"$repo_root/scripts/toolchain.sh" go build -o "$scratch/fry-member" ./cmd/fry-member
"$repo_root/scripts/toolchain.sh" go build -o "$scratch/fry-migrate" ./cmd/fry-migrate
"$repo_root/scripts/toolchain.sh" go build -o "$scratch/fry-s3-fixture" ./internal/testutil/cmd/s3fixture
python3 -m scripts.smoke.replica_processes "$scratch" "$container"
