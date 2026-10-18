#!/usr/bin/env bash
# Explicit opt-in to a small, billable real-AWS probe. Never provisions resources.
set -euo pipefail
if [[ ${1:-} == --help ]]; then
  echo 'Usage: scripts/ops/validate-aws-s3.sh CONFIG_JSON EXACT_BUCKET /ABSOLUTE/NEW_MANIFEST.json'
  echo 'Uses two temporary-role profiles against a dedicated sa-east-1 validation bucket.'
  echo 'Writes synthetic objects and leaves markers for separate operator cleanup.'
  exit 0
fi
if (( $# != 3 )); then
  echo 'Usage: scripts/ops/validate-aws-s3.sh CONFIG_JSON EXACT_BUCKET /ABSOLUTE/NEW_MANIFEST.json' >&2
  exit 2
fi
config_path=$1
bucket_ack=$2
manifest_path=$3
if [[ ! -f "$config_path" || "$manifest_path" != /* || -e "$manifest_path" || -L "$manifest_path" ]]; then
  echo 'Require an existing configuration and an absolute, nonexistent manifest file.' >&2
  exit 2
fi
config_path=$(realpath -- "$config_path")
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
umask 077
export FRY_AWS_S3_VALIDATION_CONFIG="$config_path"
export FRY_AWS_S3_VALIDATION_BUCKET="$bucket_ack"
export FRY_AWS_S3_VALIDATION_MANIFEST="$manifest_path"
export AWS_IGNORE_CONFIGURED_ENDPOINT_URLS=true
exec "$repo_root/scripts/toolchain.sh" test-go test -count=1 -timeout=6m -run '^TestAWSS3Validation$' -v ./internal/objectstore
