#!/usr/bin/env bash
# Only the explicit build action downloads sources or invokes docker build.
set -euo pipefail
image_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
source "$image_dir/pins.env"
kind=$1 tag=$2 recipe=$3
go_version=$(awk '$1 == "go" { print $2 }' "$image_dir/../go.mod")
[[ $(go env GOVERSION) == "go$go_version" ]] || { echo "Go $go_version required" >&2; exit 2; }
cache="${XDG_CACHE_HOME:-$HOME/.cache}/fry/testdeps"
mkdir -p "$cache"
scratch=$(mktemp -d "$cache/build.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
fetch() {
  local url=$1 checksum=$2 destination=$3
  if [[ ! -f "$destination" ]]; then
    curl --fail --location --proto '=https' --tlsv1.2 "$url" -o "$scratch/download"
    printf '%s  %s\n' "$checksum" "$scratch/download" | sha256sum -c - >&2
    mv -- "$scratch/download" "$destination"
  fi
  printf '%s  %s\n' "$checksum" "$destination" | sha256sum -c - >&2
}
case "$kind" in
  postgres)
    # --pull=false still pulls a missing FROM image. Require explicit base setup.
    docker image inspect "$FRY_TESTDEPS_BASE" >/dev/null || {
      echo "Pinned Linux base unavailable locally; explicitly provision $FRY_TESTDEPS_BASE before building" >&2
      exit 2
    }
    archive="$cache/postgresql-$FRY_TESTDEPS_POSTGRES_VERSION.tar.bz2"
    fetch "https://ftp.postgresql.org/pub/source/v$FRY_TESTDEPS_POSTGRES_VERSION/postgresql-$FRY_TESTDEPS_POSTGRES_VERSION.tar.bz2" "$FRY_TESTDEPS_POSTGRES_SHA256" "$archive"
    cp -- "$archive" "$scratch/postgresql.tar.bz2"
    cp -- "$image_dir/entrypoint.sh" "$scratch/entrypoint.sh"
    cp -- "$image_dir/postgres.Dockerfile" "$scratch/Dockerfile"
    args=(--build-arg "BASE=$FRY_TESTDEPS_BASE"
      --build-arg "DEBIAN_SNAPSHOT=$FRY_TESTDEPS_DEBIAN_SNAPSHOT"
      --build-arg "DEBIAN_SECURITY_SNAPSHOT=$FRY_TESTDEPS_DEBIAN_SECURITY_SNAPSHOT"
      --build-arg "POSTGRES_SHA256=$FRY_TESTDEPS_POSTGRES_SHA256")
    ;;
  ryuk)
    archive="$cache/ryuk-$FRY_TESTDEPS_RYUK_COMMIT.tar.gz"
    fetch "https://codeload.github.com/testcontainers/moby-ryuk/tar.gz/$FRY_TESTDEPS_RYUK_COMMIT" "$FRY_TESTDEPS_RYUK_SHA256" "$archive"
    mkdir "$scratch/source"
    tar -xzf "$archive" --strip-components=1 -C "$scratch/source"
    (cd -- "$scratch/source"
      export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly CGO_ENABLED=0 GOOS=linux
      go mod download
      go mod verify
      go build -trimpath -buildvcs=false -o "$scratch/ryuk" .)
    cp -- "$image_dir/ryuk.Dockerfile" "$scratch/Dockerfile"
    rm -rf -- "$scratch/source"
    args=()
    ;;
  *) echo "Unknown dependency image: $kind" >&2; exit 2 ;;
esac
# Docker's built-in builder avoids pulling a separate BuildKit/helper image.
DOCKER_BUILDKIT=0 docker build --pull=false --tag "$tag" \
  --label "io.fry.testdeps.recipe=$recipe" --label "io.fry.testdeps.kind=$kind" \
  "${args[@]}" "$scratch"
