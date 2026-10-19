# Hosted CI recovery — 2026-10-18

The [Linux amd64 Build & test run 39182056479](https://github.com/cotocisternas/fry/actions/runs/39182056479)
passed at commit `bcfd0e12f415c2a655f44c5d79290aa9140b5264`. Its `Validate`
step ran the complete `make check` suite in 13m31s; `Package container` passed
in 52s; the job finished in 16m40s, from 2026-10-19 00:43:33 to 01:00:13 UTC
(the evening of October 18 in America/Santiago). Mise, dependency, build and
Chromium caches restored successfully; the build cache used the preceding
revision's compatible restore key and was saved under the new commit afterward.
The workflow explicitly pulled the pinned
upstream PostgreSQL/Ryuk test images and the Distroless runtime base. It built
four native Linux amd64 executables for the runtime image and checked `fry -help`.
The [fixture reference](../../tooling/testdeps/README.md) and
[container packaging record](local-containers.md) describe those current paths.

The recovery addressed four independent failures:

- Commit `91173e8` derives `go test -coverpkg` from the same explicit root and
  local mail-module mappings as the coverage reporter. A checkout-local
  `GOMODCACHE`, as used in CI, no longer adds unrelated downloaded modules to
  the measured profile. See [coverage accounting](coverage-accounting.md).
- Commit `109b0e2` settles read-pin registration after cancellation once the
  database commit begins, so cancellation alone does not turn an in-flight
  commit into unknown registration debt. A database timeout can still leave
  an uncertain outcome.
- Commit `b6640cc` applies IMAP command state, capability changes and stream
  closure before notifying the waiting caller; logout remains terminal after a
  concurrent write failure. The patched mail-library race suite passed in the
  final local and hosted validation.
- Commit `bcfd0e1` detects Docker CLI and client/server API support before
  using `image inspect --platform`. For Docker 28/API 1.48 it inspects the
  locally cached default image and checks its actual OS/architecture. The
  earlier [hosted run 39177730860](https://github.com/cotocisternas/fry/actions/runs/39177730860)
  had passed validation but failed packaging on that unsupported flag.

The matching-tree local `make check`, `make container IMAGE=fry:ci ARCH=amd64`
and image inspector passed with `GOMAXPROCS=2`, `taskset -c 0,1`, CI-style
caches, a Docker 28.0.4 CLI using API 1.48, and a Docker 29.9.1 daemon. Logs
and source freeze checks are under
`.artifacts/ci-failure-39177730860/final-round3/`. This proves the older
client/newer daemon combination locally; the hosted success supplies the
Docker 28 runner result. Neither local result is a Docker 28 daemon test.

This is Linux amd64 CI and packaging evidence. Native arm64 execution,
release/recovery qualification and live AWS/IAM validation remain open gates.
The [local Kubernetes rehearsal](local-kubernetes.md) remains security-held and
was not part of this run.
