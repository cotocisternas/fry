#!/usr/bin/env bash
# Public benchmark dispatch; each Python module owns its complete workflow.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
case_name=${1:-help}
if (( $# )); then shift; fi
case "$case_name" in
  help|-h|--help)
    cat <<'USAGE'
Usage: scripts/bench.sh CASE [ARGS]

  acceptance baseline|profile|trace NEW_OUTPUT_DIRECTORY [legacy|bounded] [65536|1048576]
  hotstore-retained NEW_OUTPUT_DIRECTORY [RECORDS MESSAGE_BYTES]
  load-mail [COUNT [MESSAGE_BYTES [CONCURRENCY [NEW_REPORT_JSON [REPLICA_PAIRS]]]]]
  mailbox-publication [NEW_REPORT_JSON]
  retained-reclaim [RECORDS [MESSAGE_BYTES [NEW_REPORT_JSON]]]
  query-plans [NEW_REPORT_JSON]

All workloads use fresh synthetic local data. Run make setup first.
USAGE
    exit 0 ;;
  acceptance|hotstore-retained|load-mail|mailbox-publication|retained-reclaim|query-plans)
    module="scripts.bench.${case_name//-/_}" ;;
  *) echo "Unknown benchmark '$case_name'; run scripts/bench.sh --help" >&2; exit 2 ;;
esac
cd "$repo_root"
export PYTHONDONTWRITEBYTECODE=1
exec "$repo_root/scripts/toolchain.sh" exec python3 -m "$module" "$@"
