#!/usr/bin/env bash
# Build on the host with pinned Go, then send only prepared outputs to Docker.
set -euo pipefail
[[ $# -le 2 ]] || { echo 'Usage: scripts/container.sh [IMAGE] [amd64|arm64]' >&2; exit 2; }
image=${1:-fry:local-test}
arch=${2:-}
if [[ -z $arch ]]; then
  case $(uname -m) in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
  esac
fi
case $arch in
  amd64|arm64) ;;
  *) echo 'container: ARCH must be amd64 or arm64.' >&2; exit 2 ;;
esac
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$repo_root"
for dependency in docker python3 timeout; do
  command -v "$dependency" >/dev/null || { echo "container: $dependency is required." >&2; exit 2; }
done
# Resolve the one runtime pin from the packaging recipe; never implicitly pull.
base=$(awk '$1 == "FROM" {print $2}' Dockerfile)
if python3 "$repo_root/tooling/container/image_inspect.py" "$base" "$arch" >/dev/null; then
  :
else
  status=$?
  if [[ $status == 3 ]]; then
    printf 'container: cache the pinned runtime explicitly, then retry:\n  docker pull --platform linux/%s %s\n' "$arch" "$base" >&2
  fi
  exit 2
fi
# Ignore personal Go settings before validating/using the pinned native compiler.
export GOENV=off
unset GOROOT
source "$repo_root/scripts/toolchain.sh"
fry_ensure go
mkdir -p "$repo_root/.artifacts/container"
context=$(mktemp -d "$repo_root/.artifacts/container/context.XXXXXXXX")
trap 'rm -rf -- "$context"' EXIT
cp "$repo_root/Dockerfile" "$repo_root/.dockerignore" "$context/"
fry_exec_at "$repo_root" python3 "$repo_root/tooling/container/prepare.py" \
  "$repo_root" "$context" "$arch" "$(fry_binary go)"
timeout 600 docker build --platform "linux/$arch" --pull=false --network=none \
  --build-arg "TARGETARCH=$arch" --tag "$image" "$context"
python3 "$repo_root/tooling/container/image_inspect.py" "$image" "$arch" >/dev/null
printf 'Packaged %s (linux/%s); no application image was pulled or pushed.\n' "$image" "$arch"
