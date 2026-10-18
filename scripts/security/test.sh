#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
export PYTHONDONTWRITEBYTECODE=1
cd "$repo_root"
python3 -m unittest discover -s tooling/security -p 'test_*.py'
