#!/usr/bin/env bash
# The same contributor gate locally and in CI; never silently narrow selection.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$repo_root"
evidence=.artifacts/contributor-checks
mkdir -p "$evidence"
rm -f "$evidence/PASSED"
[[ $# == 0 ]] || { echo 'check accepts no test-selection arguments.' >&2; exit 2; }
for setting in TEST_FLAGS FRY_TEST_PACKAGES FRY_TEST_RUN FRY_COVERAGE_FLOORS FRY_COVERAGE_DIR \
  FRY_TEST_DATABASE_URL FRY_TEST_POSTGRES_CONTAINER; do
  if [[ -n ${!setting:-} ]]; then
    echo "check refuses $setting; use focused targets for partial verification." >&2
    exit 2
  fi
done
snapshot() {
  # Git represents symlink text and executable bits without dereferencing the
  # shared skill-directory links. Include HEAD so a changed base cannot hide edits.
  {
    git rev-parse HEAD
    git --no-pager diff --no-ext-diff --no-textconv --no-color --binary HEAD --
  } | sha256sum
}
snapshot > "$evidence/source.before.sha256"
for action in lint-config lint-policy-check lint fmt-check vet build sqlc-check sqlc-vet proto-check test-deps-lint test-scripts; do
  ./scripts/quality.sh "$action"
done
./scripts/test-deps.sh test
FRY_COVERAGE_DIR="$evidence" ./scripts/quality.sh coverage -short
./scripts/quality.sh test-forks -race
./scripts/quality.sh test-browser
./scripts/quality.sh security
snapshot > "$evidence/source.after.sha256"
cmp "$evidence/source.before.sha256" "$evidence/source.after.sha256"
printf '%s\n' 'Contributor checks passed (Linux native, root race/short with PostgreSQL, full fork race suites, browser, security, API compatibility and generated drift).' > "$evidence/PASSED"
cat "$evidence/PASSED"
