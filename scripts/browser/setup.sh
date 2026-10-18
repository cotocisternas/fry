#!/usr/bin/env bash
# Explicit dependency installation; ordinary browser checks never download.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
case "${1:-}" in
  -h|--help)
    echo 'Usage: scripts/browser/setup.sh [--with-deps]'
    echo 'Install locked Playwright and Chromium; --with-deps also installs Linux libraries.'
    exit 0
    ;;
  '') [[ $# == 0 ]] || { echo 'setup-browser: unexpected arguments' >&2; exit 2; } ;;
  --with-deps) [[ $# == 1 ]] || { echo 'setup-browser: unexpected arguments' >&2; exit 2; } ;;
  *) echo 'Usage: scripts/browser/setup.sh [--with-deps]' >&2; exit 2 ;;
esac
cd "$repo_root"
source "$repo_root/scripts/toolchain.sh"
fry_ensure node
export CI=1 PLAYWRIGHT_BROWSERS_PATH="$repo_root/.artifacts/browser/browsers"
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
fry_exec_at "$repo_root" npm ci --prefix scripts/browser --include=dev --ignore-scripts --no-audit --no-fund
unset PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD
fry_exec_at "$repo_root" node scripts/browser/node_modules/playwright/cli.js install --only-shell "${@}" chromium
sha256sum scripts/browser/package.json scripts/browser/package-lock.json > scripts/browser/node_modules/.fry-inputs.sha256
