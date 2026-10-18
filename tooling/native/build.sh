#!/usr/bin/env bash
# Sourced by setup.sh. Outputs stay in its disposable staging directory.
# shellcheck disable=SC2154 # repo_root/native_root are supplied by toolchain.sh.
fry_build_native() {
  local destination=$1 tool module package version flags build_arch copy
  build_arch=$("$(fry_binary go)" env GOHOSTARCH)
  for tool in golangci-lint sqlc; do
    module="$repo_root/tooling/native/$tool"
    copy="$destination/../modules-native/$tool"
    mkdir -p "$copy"
    cp "$module/"{go.mod,go.sum} "$copy/"
    package=$(awk '$1 == "tool" {print $2}' "$module/go.mod")
    version=$(awk -v path="${package%/cmd/*}" '$1 == path {print $2}' "$module/go.mod")
    flags='-s -w'
    if [[ $tool == golangci-lint ]]; then
      # Stable labels identify a local security rebuild, never an official binary.
      flags+=" -X main.version=${version#v} -X main.commit=fry-source -X main.date=source-build"
    fi
    # Ignore personal Go settings and disable native C/toolchain variability.
    # go.sum and the public checksum database authenticate the downloaded source.
    local -a build_env=(env GOENV=off GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly
      CGO_ENABLED=0 GOOS=linux "GOARCH=$build_arch" GOAMD64=v1 GOARM64=v8.0 GOEXPERIMENT= GOFIPS140=off
      "GOMODCACHE=$native_root/go-mod" GOPROXY=https://proxy.golang.org
      GOSUMDB=sum.golang.org GOPRIVATE= GONOSUMDB= GONOPROXY= GOINSECURE=)
    fry_exec_at "$copy" "${build_env[@]}" go mod download
    fry_exec_at "$copy" "${build_env[@]}" go mod verify
    if [[ $tool == golangci-lint ]]; then
      python3 "$repo_root/tooling/native/provenance.py" lint-graph "$repo_root" \
        "$native_root/go-mod/${package%/cmd/*}@$version/go.mod"
    fi
    fry_exec_at "$copy" "${build_env[@]}" go build -trimpath -buildvcs=false \
      -ldflags "$flags" -o "$destination/$tool" "$package"
    cmp -s "$module/go.mod" "$copy/go.mod" || fry_fail "$tool/go.mod needs an explicit dependency update."
    cmp -s "$module/go.sum" "$copy/go.sum" || fry_fail "$tool/go.sum needs an explicit dependency update."
    python3 "$repo_root/tooling/native/provenance.py" record "$repo_root" "$tool" \
      "$(fry_binary go)" "$destination/$tool" "$(fry_pin go)" "$FRY_NATIVE_BUILD_SCHEMA"
  done
}
