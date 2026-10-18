#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"
tool="$repo_root/scripts/toolchain.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
incomplete() { echo "INCOMPLETE: $*" >&2; exit 2; }
fork_tests() {
  local module
  for module in go-imap go-smtp; do
    (cd "$repo_root/third_party/$module"; "$tool" test-go test -count=1 "-timeout=${FRY_TEST_TIMEOUT:-120s}" "$@" ./...)
  done
}
case "$action" in
  fmt|fmt-check)
    # Explicit roots protect nested upstream forks from formatter writes.
    args=()
    if [[ "$action" == fmt-check ]]; then args+=(--diff); fi
    "$tool" lint fmt --config "$repo_root/.golangci.yml" "${args[@]}" ./cmd/... ./internal/... ./db/...
    (cd "$repo_root/tooling/testdeps"; "$tool" lint fmt --config "$repo_root/.golangci.yml" "${args[@]}" ./...)
    ;;
  lint)
    # JSON on stdout allows reports anywhere on the host without extra mounts:
    # scripts/quality.sh lint --output.json.path stdout --output.text.path stderr > /tmp/lint.json
    "$tool" lint run --config "$repo_root/.golangci.yml" --show-stats=false "$@" ./...
    ;;
  lint-config) "$tool" lint config verify --config "$repo_root/.golangci.yml" ;;
  lint-policy-check)
    PYTHONDONTWRITEBYTECODE=1 "$tool" exec python3 -m scripts.tests.lint_policy
    ;;
  test-script-processes)
    PYTHONDONTWRITEBYTECODE=1 "$tool" exec python3 -m scripts.tests.processes
    ;;
  test-scripts)
    PYTHONDONTWRITEBYTECODE=1 "$tool" exec python3 -m unittest discover -s scripts/tests -t . -p '*.py'
    PYTHONDONTWRITEBYTECODE=1 "$tool" exec python3 -m unittest discover -s tooling/container -p 'test_*.py'
    ;;
  test-deps-lint)
    (cd "$repo_root/tooling/testdeps"; "$tool" lint run --config "$repo_root/.golangci.yml" ./...)
    (cd "$repo_root/tooling/testdeps"; "$tool" go vet ./...)
    ;;
  build) "$tool" go build ./cmd/... ;;
  vet)
    "$tool" go vet ./...
    for module in go-imap go-smtp; do
      (cd "$repo_root/third_party/$module"; "$tool" go vet ./...)
    done
    ;;
  test)
    echo 'Lightweight local tests: PostgreSQL cases can skip; this is incomplete verification.' >&2
    "$tool" test-go test -count=1 "-timeout=${FRY_TEST_TIMEOUT:-120s}" "$@" ./...
    fork_tests "$@"
    ;;
  test-integration) "$tool" receipts "$@"; fork_tests "$@" ;;
  test-race) "$tool" receipts -race "$@"; fork_tests -race "$@" ;;
  test-forks) fork_tests "$@" ;;
  coverage|coverage-report)
    coverage_dir=${FRY_COVERAGE_DIR:-.artifacts/coverage}
    mkdir -p "$coverage_dir"
    coverage_dir=$(cd -- "$coverage_dir" && pwd -P)
    [[ "$coverage_dir" == "$repo_root/"* ]] || incomplete 'Coverage outputs must be inside the checkout for auditable local evidence.'
    profile="$coverage_dir/root.out"
    report="$coverage_dir/root.json"
    if [[ "$action" == coverage ]]; then
      # Root tests can instrument local replacement dependencies. Keep their
      # records in the raw profile and report them separately from root floors;
      # they do not replace execution of the complete fork suites.
      # Invalidate prior evidence before running; a failed test run must not
      # leave an old successful JSON report beside a new partial profile.
      : > "$profile"
      : > "$report"
      FRY_TEST_TIMEOUT=${FRY_TEST_TIMEOUT:-5m} "$tool" receipts "$@" \
        -race -covermode=atomic -coverpkg=./... "-coverprofile=$profile"
    fi
    report_args=(-repo "$repo_root" -profile "$profile"
      -module github.com/emersion/go-imap/v2=third_party/go-imap
      -module github.com/emersion/go-smtp=third_party/go-smtp)
    report_args+=(-floors "${FRY_COVERAGE_FLOORS:-$repo_root/tooling/coverage-floors.json}")
    "$tool" go run ./internal/devtools/coverage "${report_args[@]}" > "$report"
    echo "Root coverage profile: $profile"
    echo "Root coverage report: $report"
    ;;
  generate|generate-check)
    mode=generate
    if [[ "$action" == generate-check ]]; then mode=check; fi
    "$repo_root/scripts/generate/sqlc.sh" "$mode"
    "$repo_root/scripts/generate/proto.sh" "$mode" proto/admin gen/admin
    "$repo_root/scripts/generate/proto.sh" "$mode" proto/member gen/member
    "$repo_root/internal/admintransport/build-web.sh" "$mode"
    ;;
  sqlc) "$repo_root/scripts/generate/sqlc.sh" generate ;;
  sqlc-check) "$repo_root/scripts/generate/sqlc.sh" check ;;
  proto-drift-check)
    "$repo_root/scripts/generate/proto.sh" check proto/admin gen/admin
    "$repo_root/scripts/generate/proto.sh" check proto/member gen/member
    "$repo_root/internal/admintransport/build-web.sh" check
    ;;
  sqlc-vet) "$repo_root/scripts/generate/sqlc.sh" vet ;;
  test-browser)
    "$repo_root/scripts/browser/test.sh" "$@"
    "$tool" exec python3 -m scripts.browser.lifecycle_test
    ;;
  security)
    "$repo_root/scripts/security/test.sh"
    "$repo_root/scripts/security/check.sh" "$@"
    ;;
  proto-check)
    "$repo_root/scripts/quality.sh" proto-drift-check
    "$repo_root/tooling/rpc/verify-compatibility.sh"
    "$repo_root/scripts/generate/compatibility.sh"
    ;;
  check) "$repo_root/scripts/tests/check.sh" "$@" ;;
  release-check) incomplete 'Release gate is unavailable: native amd64/arm64 qualification, release artifacts/provenance and load/failure rehearsals remain incomplete.' ;;
  help)
    echo 'make demo; make smoke SCENARIO=smtp (scripts/smoke.sh --help lists scenarios)'
    echo 'scripts/bench.sh --help (opt-in measurements)'
    echo 'Script responsibilities and direct commands: scripts/README.md'
    echo 'make setup (mise native tools + pinned upstream test dependency images)'
    echo 'make setup-tools doctor test-deps-pull test-deps-check test-deps-test test-deps-lint native-tools-check'
    echo 'make test-scripts (Docker-free script modules and lifecycle regressions)'
    echo 'make test-script-processes (focused subprocess cleanup regressions)'
    echo 'make lint-policy-check (prove style checks cover handwritten production and test code)'
    echo 'make fmt fmt-check lint lint-config build vet generate generate-check sqlc-vet'
    echo 'make test test-integration test-race test-forks toolchain-versions'
    echo 'make baseline-check (existing-code baseline; not the complete PR gate)'
    echo 'make proto-bootstrap proto-tools-check proto-drift-check (generation checks; not released API compatibility)'
    echo 'make coverage coverage-report'
    echo 'make container IMAGE=fry:local-test ARCH=amd64 (native binaries, distroless packaging)'
    echo 'Native mise tools; Docker runs dependencies and deployment rehearsals. TEST_FLAGS=-short for short suites.'
    echo 'make browser-setup security-setup (explicit downloads for contributor checks)'
    echo 'make check (complete contributor gate); make release-check remains unavailable.'
    ;;
  *) echo "Unknown quality action: $action" >&2; exit 2 ;;
esac
