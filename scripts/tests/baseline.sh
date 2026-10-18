#!/usr/bin/env bash
# Existing-code baseline only. Browser/security/API-compatibility/release gates are separate.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$repo_root"
for setting in TEST_FLAGS FRY_TEST_PACKAGES FRY_TEST_RUN FRY_COVERAGE_FLOORS FRY_COVERAGE_DIR; do
  if [[ -n ${!setting:-} ]]; then
    echo "baseline-check refuses $setting: use individual targets for partial or custom verification." >&2
    exit 2
  fi
done
[[ $# == 0 ]] || { echo 'baseline-check accepts no test-selection arguments.' >&2; exit 2; }
evidence=.artifacts/baseline-check
mkdir -p "$evidence"
rm -f "$evidence/PASSED"
snapshot() {
  find cmd internal db proto gen third_party tooling scripts \
    -type d -name __pycache__ -prune -o -type f -print0 | sort -z | xargs -0 sha256sum
  sha256sum go.mod go.sum sqlc.yaml .golangci.yml Makefile Dockerfile .dockerignore mise.toml mise.lock
}
snapshot > "$evidence/source.before.sha256"
for action in lint-config lint-policy-check lint fmt-check vet build generate-check sqlc-vet test-deps-lint test-scripts; do
  ./scripts/quality.sh "$action"
done
./scripts/test-deps.sh test
FRY_COVERAGE_DIR="$evidence" ./scripts/quality.sh coverage -short
./scripts/quality.sh test-forks -race
snapshot > "$evidence/source.after.sha256"
cmp "$evidence/source.before.sha256" "$evidence/source.after.sha256"
printf '%s\n' 'Existing-code baseline passed: native core -race -short, reviewed floors, complete fork race suites, lint/format/vet/build, SQL/RPC/browser generation and fixture lifecycle tests; unchanged source inputs.' > "$evidence/PASSED"
cat "$evidence/PASSED"
