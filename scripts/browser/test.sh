#!/usr/bin/env bash
# Required Chromium gate; setup is a separate, explicit download boundary.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
case "${1:-}" in
  -h|--help) echo 'Usage: scripts/browser/test.sh (runs both Chromium suites against a fresh demo)'; exit 0 ;;
esac
[[ $# == 0 ]] || { echo 'test-browser accepts no test-selection arguments.' >&2; exit 2; }
cd "$repo_root"
if [[ ! -f scripts/browser/node_modules/.fry-inputs.sha256 ]] ||
   ! sha256sum --check --status scripts/browser/node_modules/.fry-inputs.sha256; then
  echo 'test-browser: missing or stale browser dependencies; run scripts/browser/setup.sh.' >&2
  exit 2
fi
# All native module inputs must have been prepared by repository setup.
export GOPROXY=off GOSUMDB=off
exec "$repo_root/scripts/toolchain.sh" exec python3 -m scripts.browser.run
