#!/usr/bin/env bash
# The native generation boundary must work with Docker entirely unavailable.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"
mkdir -p .artifacts/native-tooling
scratch=$(mktemp -d "$repo_root/.artifacts/native-tooling/verify.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
mkdir "$scratch/bin"
cat > "$scratch/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo 'Native generation attempted to invoke Docker.' >&2
exit 99
STUB
chmod +x "$scratch/bin/docker"
export PATH="$scratch/bin:$PATH"
export DOCKER_HOST="unix://$scratch/no-docker.sock"
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tooling/native -p 'test_*.py'
./tooling/native/check-buf.sh
./scripts/generate/sqlc.sh check
./scripts/generate/sqlc.sh vet
./scripts/generate/proto.sh check proto/admin gen/admin
./scripts/generate/proto.sh check proto/member gen/member
./internal/admintransport/build-web.sh check
./tooling/rpc/verify.sh
echo 'Native generation, drift detection and output ownership checks passed without Docker.'
