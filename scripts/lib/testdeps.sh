#!/usr/bin/env bash
# Re-enter scripts once inside the pinned native tools and a fresh fixture.
export PYTHONDONTWRITEBYTECODE=1
fry_testdeps_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
fry_native_run() {
  local script
  script=$(realpath -- "${BASH_SOURCE[1]}")
  if [[ ${FRY_SCRIPT_NATIVE:-} != "$script" ]]; then
    exec "$fry_testdeps_root/scripts/toolchain.sh" exec env FRY_SCRIPT_NATIVE="$script" bash "$script" "$@"
  fi
}

fry_testdeps_run() {
  local script
  script=$(realpath -- "${BASH_SOURCE[1]}")
  if [[ ${FRY_SCRIPT_FIXTURE:-} != "$script" ]]; then
    exec "$fry_testdeps_root/scripts/test-deps.sh" run -- \
      "$fry_testdeps_root/scripts/toolchain.sh" exec env FRY_SCRIPT_FIXTURE="$script" bash "$script" "$@"
  fi
  [[ -n ${FRY_TEST_DATABASE_URL:-} && -n ${FRY_TEST_POSTGRES_CONTAINER:-} ]] || {
    echo 'Shared Testcontainers fixture descriptor is missing' >&2
    exit 1
  }
}
