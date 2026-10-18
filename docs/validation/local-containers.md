# Native binaries and local runtime containers

FRY compiles on the host with the Go version pinned in `mise.toml`. Docker only
packages the prepared Linux executables into the existing digest-pinned
Distroless static Debian 13 nonroot base. There is no compiler stage or compiler
tool image in this workflow.

## Explicit preparation and packaging

Run `./scripts/setup.sh` to prepare native tools and dependency caches. Prepare
the runtime base separately for the intended architecture:

```sh
docker pull --platform linux/amd64 \
  gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
make container IMAGE=fry:local-test ARCH=amd64
python3 tooling/container/check.py fry:local-test amd64
```

Use `arm64` in all three commands to package that architecture. An omitted
`ARCH` selects the native host architecture; an omitted `IMAGE` selects
`fry:local-test`. The host must be Linux amd64 or arm64, with Docker, Python 3,
GNU `timeout`, mise, and the pinned Go installation available. Docker must support
platform-specific local image inspection. Cross-compilation uses native Go;
execution checks run only when the image architecture matches the host.

`scripts/container.sh` requires the selected base platform to be cached locally
and prints its exact pull command when missing. It never pulls or publishes an
application image. Go uses existing dependency caches with `GOPROXY=off` and
`GOSUMDB=off`; missing dependency sources fail instead of downloading them during
packaging. Run the explicit native setup step to populate missing caches.

## Prepared context and runtime contract

Each invocation creates a fresh private context under `.artifacts/container/`
and removes it on exit. Only the packaging recipe, allowlist, four binaries,
license notices and an empty data directory enter it. Repository source, local
configuration, fixture data, credentials and preexisting `bin/` output are not
Docker inputs. The root Dockerfile is a packaging recipe consumed by this
wrapper; invoking it against the source checkout has no prepared runtime inputs.

The four commands are `fry`, `fry-member`, `fry-migrate`, and `fry-admin` under
`/usr/local/bin/`. Native preparation forces `CGO_ENABLED=0`, Linux,
`-mod=readonly`, `-trimpath`, `-buildvcs=false`, and baseline amd64/arm64 settings.
It clears personal Go flags, workspaces, compiler-root overrides and experiment
settings. ELF headers and Go build metadata must match the requested architecture
and command, and no dynamic loader or dynamic segment is permitted. Existing
output directories are rejected, and module manifests are compared before and
after native preparation.

Notices come from the modules used by `go list -deps -json` for those four
commands, deduplicated by module path. Locally patched replacements supply their
own notices under the original module names. The repository license and Go's
standard-library license/patent notices are included under
`/usr/share/fry/licenses/`. All notice directories are normalized to 0755 and
notice files to 0644, including under a restrictive host umask, so UID 65532 can
read them after Docker copies them as root-owned files. The context and data
directory retain their private permissions. Unused lazy graph dependencies are not downloaded or
packaged. The original runtime index digest is unchanged.

The image runs as numeric UID/GID 65532. `/var/lib/fry` is owned by that identity
with mode 0700; the Distroless system TLS roots remain present. The exec
entrypoint defaults to `fry -help`. Select another command with Docker's
`--entrypoint /usr/local/bin/fry-member` or the equivalent runtime setting.
Member data and any configured packing scratch directory still require their
appropriate writable persistent mounts.

## Bounded validation and recorded result

```sh
python3 -m unittest discover -s tooling/container -p 'test_*.py'
bash -n scripts/container.sh
python3 tooling/container/check.py fry:local-test amd64
```

The 19 focused regressions reject foreign/dynamic/truncated binaries, stale output
directories, incomplete command sets, context secrets/data/symlinks, personal Go
configuration, and unavailable notice sources. They also check local replacement
notices, restrictive-umask notice readability, missing-base/unsupported-architecture
failures and image-check cleanup failures without Docker.
The image checker exports a temporary container filesystem, verifies binaries,
notices, system TLS roots and data ownership, then starts all four commands with
`-help`, a read-only root, no network, dropped capabilities and
`no-new-privileges`. Every checker-created container is explicitly removed with
checked results before success is reported. Go build and Docker packaging each have a ten-minute bound;
metadata commands have three-minute bounds; image inspection/help commands have
30-second bounds and filesystem export has a 60-second bound.

On 2026-10-18 the native Linux/amd64 build and image checks passed for local tag
`fry:native-container-20261018`, built with Go 1.27.2:

- Local index after the final `umask 077` rebuild: `sha256:f619bcd61e7a717159279523303501ae106a2040cb0b18f530c35cd2c539428d`.
- Platform manifest: `sha256:527d55713d517b9c97206bf5c77c9633e69b33fd50568a113558caa9f677b293`.
- Image config: `sha256:db6339de2837a772be6c77466fa6461ed4ed790d7c0b753996e79f9c240bfa01`.

Packaging also passed with ambient `GOFLAGS` set to conflicting race/tag flags;
the script replaced those flags. The final linked-module-notice build left root
and replacement-module manifests unchanged. Only the pinned runtime base was
explicitly pulled; no application image was pulled or pushed. Linux/arm64 is
covered by preparation/ELF regression checks; no arm64 runtime execution is
claimed. Shell syntax and ShellCheck 0.11.0 `-x` checks passed using the installed
ShellCheck binary directly. The final build ran under `umask 077`, and image
inspection verified the complete notice tree remains readable by the nonroot
runtime identity. The platform manifest and image config stayed identical to
the earlier default-umask build; only the index's build attestation changed.

The parent run of `./scripts/smoke.sh containers fry:native-container-20261018`
passed the full runtime flow using the preceding local index
`sha256:13a8f00f9fd239cb998185d37adac9db17d30a2c45e342af9d30ce8dec4ecd2c`,
with the same platform manifest and config listed above. It verified TLS mail,
exact original retrieval, clean restart on retained volumes, survivor reads,
refusal to accept new mail with a member down, unchanged fenced owner state on
unclean restart, and checked cleanup. Its local log is
`.artifacts/ci-setup/native-container-smoke-final.log`. The later restrictive-umask
packaging rebuild and the updated image checker passed separately; the
PostgreSQL rehearsal was not duplicated for that rebuild.

These checks establish packaging and command startup. The separate
[container runtime rehearsal](container-runtime.md) covers PostgreSQL, TLS mail,
retained volumes and crash fencing; the historical results there retain their
original image identities and build method. Neither record establishes
production deployment or failure-domain independence.
