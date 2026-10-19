# Source-built test image remediation — 2026-10-18

> Historical evidence: the user subsequently clarified that upstream service
> images are permitted. The active fixture now uses digest-pinned upstream
> PostgreSQL and Ryuk images; see the [fixture reference](../../tooling/testdeps/README.md).
> The scan results below apply only to the recorded source-built image IDs,
> not to those upstream images.


The PostgreSQL fixture tested in this record used authenticated Debian main **and security**
snapshots, with installed packages upgraded in both image stages. All previous
Critical matches and all OpenSSL matches are absent from the new scan. Remaining
findings are retained below. The Kubernetes rehearsal is on a security hold:
there is no verified official patched kind node digest available today.

This follows [the original audit](security-audit-2026-10-18.md); it does not
rewrite that audit's historical results. Local evidence is under
`.artifacts/image-remediation-2026-10-18/`. No Git commit, publication, global
Docker cleanup, or prebuilt service/helper image download was performed.

## Inputs and build

The only provisioned image was the explicitly pulled official Linux base:
`debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f`.
Its multi-platform digest matches the [official Debian registry metadata](https://hub.docker.com/v2/repositories/library/debian/tags/trixie-slim)
and Docker's verified pull result. Moving from bookworm to trixie supplies the
OpenSSL fixes that bookworm's security repository still lacks. The builder now
requires this exact base locally before downloading PostgreSQL or invoking
Docker build: `--pull=false` alone can still pull a missing `FROM` image.

Both stages use immutable timestamp **20261017T000000Z** for
[Debian main](https://snapshot.debian.org/archive/debian/20261017T000000Z/dists/trixie/InRelease)
and [Debian security](https://snapshot.debian.org/archive/debian-security/20261017T000000Z/dists/trixie-security/InRelease).
APT authenticates indexes using the explicit Debian archive keyring and verifies
package hashes. Only historical `Valid-Until` expiry is disabled; no trusted or
unauthenticated installation bypass is used. `apt-get dist-upgrade` runs before
installing dependencies in both stages. The build log confirms that the base's
`liblzma5` was upgraded from `5.8.1-1+deb13u1` to security revision `deb13u2`.

PostgreSQL **18.6** remains built from its original source archive, whose SHA-256
`555610c24d53e4316da5b7d3fc25c279d96856d5e0e23ee308c328c5fa881d9f`
matches the [official checksum](https://ftp.postgresql.org/pub/source/v18.6/postgresql-18.6.tar.bz2.sha256).
Ryuk **0.14.0** remains source-built at commit
`b3726afd6cc2c36628abcc08e9cabac43f587384`, confirmed by the
[official tag reference](https://api.github.com/repos/testcontainers/moby-ryuk/git/ref/tags/0.14.0).
Its archive SHA-256 remains
`7754e8598010a543c015c5d14b10372e15a5a00c68e5e5b81300c2cde4d2f01d`;
the build verifies its module cache and compiles with Go **1.27.2** into scratch.
No dependency override or source patch was needed for Ryuk.

After the testdeps manifest/import freeze, `scripts/test-deps.sh build` completed
once, with exactly one successful build per image. Both tags use recipe
`8d7dfdf808ddd5f1cbd3d5372680d261600e6beb65281b91d0393c5229779ecf`:

- PostgreSQL: `fry-testdeps-postgres:8d7dfdf808ddd5f1cbd3d537-amd64`, immutable ID
  `sha256:9d973e5d80ddf21c5d3f3446540ebc5e5c68843228843f0265f7cc5e9e76d70a`.
- Ryuk: `fry-testdeps-ryuk:8d7dfdf808ddd5f1cbd3d537-amd64`, immutable ID
  `sha256:708cbf724aee6d0c2ced46168503edfcdb3c2e624044f7a127c265a47fb1e77b`.

## Fresh scan and remaining findings

Trivy **0.69.3** downloaded a new dedicated database into this remediation's
`cache/`, rather than using the shared cache or the original audit cache. Schema
2 metadata records `UpdatedAt=2026-10-18T13:11:41.355239553Z` and
`DownloadedAt=2026-10-18T17:22:05.716074226Z`. Both then-current images were scanned
by full immutable ID using Docker-only image discovery, vulnerability scanning,
offline analysis, no database refresh during scans, and `/dev/null` as the
ignore file. No severity or unfixed-finding filter was applied. Scan exit 0
means completion, not zero vulnerabilities. Exact commands and exits are in
`scan-status.json`; raw JSON, logs, `findings.csv` and `summary.json` retain every
match, installed/fixed version, severity and advisory ID.

PostgreSQL now has **162 package findings / 71 unique advisories**, compared
with the original **254 / 125**. Raw severities are **0 Critical, 43 High,
58 Medium, 60 Low, 1 Unknown**; unique severities are **0 Critical, 8 High,
29 Medium, 33 Low, 1 Unknown**. The scanner identifies 81 distro packages and
zero language files. Source-installed PostgreSQL is independently verified by
the checksum and the executed `postgres (PostgreSQL) 18.6` version check.

Installed `perl-base` is **5.40.1-6+deb13u1**; the old Critical Perl matches,
including the previously unresolved CVE-2026-13221 classification, are absent.
Installed `libssl3t64` and `openssl-provider-legacy` are
**3.5.7-1~deb13u3**, with **zero OpenSSL matches**, including no CVE-2026-84782.
The [Debian tracker](https://security-tracker.debian.org/tracker/CVE-2026-84782)
identifies that trixie security revision as fixed while bookworm remains
vulnerable. `ldd` confirms the database loads the installed SSL/crypto libraries
and has no missing dynamic libraries.

All eight remaining High advisory IDs have no trixie fix listed by the scanner;
current primary Debian tracker pages corroborate the absence of a fixed trixie
package. These remain visible, without a blanket non-applicability claim:

- [CVE-2025-69720](https://security-tracker.debian.org/tracker/CVE-2025-69720):
  ncurses, repeated across `libtinfo6`, `ncurses-base` and `ncurses-bin`.
- [CVE-2026-16742](https://security-tracker.debian.org/tracker/CVE-2026-16742):
  systemd-homed signature verification, matched against `libsystemd0` and
  `libudev1`. Static image inventory contains no `systemd-homed` executable.
- [CVE-2026-54369](https://security-tracker.debian.org/tracker/CVE-2026-54369):
  `libacl1` symlink traversal.
- [CVE-2026-76642](https://security-tracker.debian.org/tracker/CVE-2026-76642),
  [CVE-2026-78408](https://security-tracker.debian.org/tracker/CVE-2026-78408),
  [CVE-2026-78409](https://security-tracker.debian.org/tracker/CVE-2026-78409),
  [CVE-2026-78410](https://security-tracker.debian.org/tracker/CVE-2026-78410):
  util-linux mount/nsenter issues repeated across nine binary packages.
- [CVE-2026-9538](https://security-tracker.debian.org/tracker/CVE-2026-9538):
  Perl Archive::Tar denial of service, Debian fix deferred. The affected
  `Archive/Tar` module is absent from this image.

The other remaining Perl matches are **CVE-2026-15534 (Medium, fix deferred)**,
**CVE-2011-4116 (Low)** and **CVE-2026-82560 (Unknown)**. Perl and `File::Temp`
are present; `Pod::Text` is absent. The shell entrypoint does not invoke Perl,
and no PLPerl plugin is installed. This is limited component/usage evidence;
it does not claim the installed interpreter is fully fixed. All remaining
Medium/Low/Unknown package matches, including linked runtime libraries, remain
in the exact CSV/JSON inventory. No remaining PostgreSQL-image match lists an
available fixed version. Further distro/no-fix triage is outstanding.

Ryuk retains exactly **four findings / four advisories: 2 High, 2 Medium** in
`github.com/docker/docker v28.3.3+incompatible`:
**CVE-2026-41567**, **CVE-2026-42306**, **CVE-2026-41568**, and **CVE-2026-33997**.
The first three describe daemon archive/copy implementations, per the official
[archive](https://github.com/moby/moby/security/advisories/GHSA-x86f-5xw2-fm2r),
[mount](https://github.com/moby/moby/security/advisories/GHSA-rg2x-37c3-w2rh),
and [empty-file](https://github.com/moby/moby/security/advisories/GHSA-vp62-88p7-qqf5)
advisories; the fourth concerns daemon
[plugin privilege validation](https://github.com/moby/moby/security/advisories/GHSA-pxq6-2prw-chj9).
Ryuk's source imports client/API packages and lists/removes resources. Its
265-package build dependency graph contains none of the affected daemon,
`pkg/archive`, `pkg/chrootarchive`, or plugin implementations. There is no
actionable affected implementation to patch in this Ryuk binary; the raw module
matches are retained. This conclusion does not clear the host Docker daemon.

## Kind security hold

The [official latest kind release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0)
still lists Kubernetes **1.37.0**. The
[official registry's 1.37 tags](https://hub.docker.com/v2/repositories/kindest/node/tags?page_size=100&name=v1.37)
contain only `v1.37.0` and `v1.37.0-rc.1`; the official `v1.37.1` tag endpoint
returns HTTP 404. The release and registry responses are saved in `sources/`.
The former digest
`a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5`
is historical blocked evidence, not a runnable fallback.

`scripts/smoke/kubernetes.py` now has `NODE=None` and exits with an explicit
security-hold message before command discovery or resource construction.
The hold requires a verified official **>=1.37.1** node digest, addressing
[CVE-2026-2270](https://github.com/kubernetes/kubernetes/issues/142097), plus
explicit local provisioning. No image is implicitly pulled and no replacement
digest was invented. No Kubernetes cluster was created or retested during this
remediation.

## Validation

- `bash -n tooling/testdeps/images/build.sh tooling/testdeps/images/pins.env`:
  passed. Python compilation of the modified runner and regression: passed.
- ShellCheck **0.11.0**, with `-x -P tooling/testdeps/images`: passed. An initial
  invocation without the source search path reported SC1091; the corrected
  invocation follows `pins.env` and has no findings.
- `python3 -m unittest scripts.tests.kubernetes -v`: **1 test passed**. It proves
  the hold fires before command execution/discovery, temporary-directory
  creation, or rehearsal construction.
- Bounded missing-base probe: builder exited **2** after exactly one mocked
  `docker image inspect`, with no build or pull. See `base-preflight.json`.
- `timeout --signal=TERM --kill-after=15s 900s ./scripts/test-deps.sh build`:
  exited **0**; the build's current-image provenance check passed.
- Two immutable-ID Trivy scans: exited **0**, with the findings above.
- `PYTHONPATH=. timeout --signal=TERM --kill-after=15s 300s python3
  .artifacts/image-remediation-2026-10-18/lifecycle.py`: exited **0** in
  **2.419 seconds**. The shared fixture first passed actual loopback TCP/SCRAM
  SQL readiness; the probe verified PostgreSQL 18.6, the configured postgres user,
  loopback port binding, a real SQL create/insert/select, installed package
  versions and dynamic-library loading. Checked runner teardown exited 0, and
  exact container `3dc82bad630e75e9f185803e73744f72edb33e2f5441ce6f3cb51e6857fd2901`
  was absent after cleanup. Only this probe's own fixture was removed.

This evidence covers Linux amd64 final images. It does not scan build-stage
inventories, arm64 images, the host, or a kind-node image. Remaining OS matches
and the unpublished patched kind node remain explicit limitations.
