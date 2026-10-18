#!/usr/bin/env bash
# One disposable PostgreSQL server for the complete root test invocation.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$repo_root"
go_command=("$repo_root/scripts/toolchain.sh" test-go)
"${go_command[@]}" version
if [[ -z ${FRY_TEST_POSTGRES_CONTAINER:-} ]]; then
  exec "$repo_root/scripts/test-deps.sh" run -- "$repo_root/scripts/tests/receipts.sh" "$@"
fi
[[ -n ${FRY_TEST_DATABASE_URL:-} ]] || { echo 'Fixture is missing FRY_TEST_DATABASE_URL.' >&2; exit 2; }
test_timeout=${FRY_TEST_TIMEOUT:-120s}
test_flags=()
read -r -a test_packages <<< "${FRY_TEST_PACKAGES:-./...}"
if [[ -n ${FRY_TEST_PACKAGES:-} ]]; then
  echo "Focused root packages (partial evidence): $FRY_TEST_PACKAGES" >&2
fi
if [[ -n ${FRY_TEST_RUN:-} ]]; then
  test_flags+=(-run "$FRY_TEST_RUN")
  echo "Focused root test selection (partial evidence): $FRY_TEST_RUN" >&2
fi
scratch=$(mktemp -d "${TMPDIR:-$repo_root}/.fry-receipt-tests.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export TMPDIR="$scratch"
docker exec "$FRY_TEST_POSTGRES_CONTAINER" postgres --version
if [[ ${FRY_TEST_STREAM:-0} == 1 ]]; then
  # -json streams events while package-list mode otherwise buffers test output.
  # pipefail preserves the Go test exit status even when rendering succeeds.
  "${go_command[@]}" test -json -count=1 "-timeout=$test_timeout" "${test_flags[@]}" "$@" "${test_packages[@]}" |
    python3 -m scripts.lib.go_test_output
else
  "${go_command[@]}" test -count=1 "-timeout=$test_timeout" "${test_flags[@]}" "$@" "${test_packages[@]}"
fi
