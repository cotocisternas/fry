#!/usr/bin/env bash
# Explicit, disposable process rehearsals. Each implementation owns its resources.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)

usage() {
  cat <<'USAGE'
Usage: scripts/smoke.sh SCENARIO [IMAGE_TAG]

Mail:       smtp, replicas, submission-credentials
Storage:    offline-restore, cold-restore, automatic-rotation, member-storage,
            schema-startup, storage-quotas
Deployment: containers, kubernetes

Only containers and kubernetes accept IMAGE_TAG (default: fry:local-test).
All scenarios use disposable synthetic data. Kubernetes creates a local kind
cluster; container images must already exist locally. Nothing is built or pulled
implicitly except native FRY test executables. AWS validation is separate.

Run make setup first. See scripts/README.md for prerequisites and scope.
USAGE
}
fail() { echo "smoke: $*" >&2; exit 2; }

scenario=${1:-help}
if (( $# )); then shift; fi
case "$scenario" in
  help|-h|--help) [[ $# == 0 ]] || fail 'help accepts no arguments'; usage; exit 0 ;;
  replicas) module='' ;;
  smtp) module=scripts.smoke.smtp ;;
  containers) module=scripts.smoke.container_processes ;;
  kubernetes) module=scripts.smoke.kubernetes ;;
  offline-restore|cold-restore|automatic-rotation|member-storage|schema-startup|storage-quotas|submission-credentials)
    module="scripts.smoke.${scenario//-/_}" ;;
  *) fail "unknown scenario '$scenario'; run scripts/smoke.sh --help" ;;
esac
if [[ $# == 1 && ( $1 == --help || $1 == -h ) ]]; then usage; exit 0; fi
case "$scenario" in
  containers|kubernetes)
    (( $# <= 1 )) || fail "$scenario accepts only an optional IMAGE_TAG"
    set -- "${1:-fry:local-test}"
    ;;
  *) [[ $# == 0 ]] || fail "$scenario accepts no arguments" ;;
esac

# Reject invalid requests before entering the toolchain or starting dependencies.
dependencies=(docker)
if [[ $scenario != smtp ]]; then dependencies+=(openssl); fi
if [[ $scenario == kubernetes ]]; then dependencies+=(kind kubectl); fi
for dependency in "${dependencies[@]}"; do
  command -v "$dependency" >/dev/null || fail "required command unavailable: $dependency"
done
cd "$repo_root"
export PYTHONDONTWRITEBYTECODE=1
if [[ -z $module ]]; then
  exec bash "$repo_root/scripts/smoke/$scenario.sh"
fi
exec "$repo_root/scripts/toolchain.sh" exec python3 -m "$module" "$@"
