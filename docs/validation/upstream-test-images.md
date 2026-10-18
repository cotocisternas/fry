# Upstream test images — 2026-10-18

The user clarified that FRY should own its Testcontainers fixture code, while
using upstream service images. This supersedes the earlier source-image build
policy. PostgreSQL and Ryuk Dockerfiles, source archives, Debian snapshots and
recipe-label checks have been removed. Native Go tool builds remain separate.

The pins and primary-source verification are in the
[fixture reference](../../tooling/testdeps/README.md). Setup explicitly pulls
them; runtime tests verify the local upstream digest/platform and create
containers from immutable IDs. FRY still owns readiness, isolation and cleanup.

## Executed checks

On Linux amd64, the revised fixture passed:

- Explicit pull and local preflight for PostgreSQL 18.6-trixie and Ryuk 0.14.0.
- Full fixture race suite, including wrong-digest/platform refusal before
  container creation, no runtime pulls, independent sessions and bounded cleanup.
- Strict fixture lint (zero findings), vet and formatting checks.
- All 46 Docker-free script tests, including four upstream-image provenance cases.
- Actual SMTP reception, authenticated submission, relay, exact IMAP bytes and
  process restart through the shared fixture.
- Offline restore after destroying the source PostgreSQL container and storage
  roots: five restored originals, one new original, two independent replicas,
  four rotations, two sealed reclaims, replay/fixed targets and two rejected
  invalid archives. PostgreSQL reported `18.6 (Debian 18.6-1.pgdg13+2)`.

Parent command logs are local ignored evidence under
`.artifacts/ci-setup/upstream-{smtp,offline-restore}.log`. They are not durable
published artifacts. Hosted CI and runtime-image packaging have separate results.
The Kubernetes security hold remains; these checks did not run Kubernetes,
native arm64 or production workloads.

## Image vulnerability inventory

The old source-image scan does not describe these upstream images. Trivy 0.69.3
scanned the actual local immutable upstream image IDs with Docker-only discovery,
all vulnerability severities, no ignore file and no unfixed filter. The scan
reused the same day's database downloaded at 17:22 UTC (database update
13:11 UTC); it did not fetch another database or perform a reachability analysis.

| Image | Raw matches | Distinct advisory IDs | Critical / high distinct IDs |
| --- | ---: | ---: | ---: |
| PostgreSQL 18.6-trixie | 377 | 179 | 2 / 40 |
| Ryuk 0.14.0 | 0 | 0 | 0 / 0 |

PostgreSQL's report contains 317 OS-package matches and 60 Go-library matches in
`/usr/local/bin/gosu`. The two critical IDs are CVE-2026-6653 in libxml2 (no fixed
package version listed) and CVE-2025-68121 in gosu's Go 1.24.6 standard library
(fixed Go versions listed). There are 64 matches with a listed fixed version:
60 in gosu and four OS-package matches. These counts describe scanner evidence,
not confirmed exploitability. No finding was silently waived, and the image is
not claimed vulnerability-free.

Full commands, image identities and unfiltered reports are local under
`.artifacts/upstream-images/`. The existing contributor security gate covers
source and selected native binaries, not OS-container inventories; this change
does not add an image-vulnerability waiver or imply a production image approval.
Upstream image updates should be reviewed and repinned through the documented
fixture upgrade process.
