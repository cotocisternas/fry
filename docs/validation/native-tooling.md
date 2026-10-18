# Native development tooling validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: passed (2026-10-18), Linux amd64 with Go 1.27.2 and PostgreSQL 18.6.

The accepted workflow uses mise-managed native Go, Node and Buf, plus locally
source-built golangci-lint, sqlc and protobuf generators. The linter/sqlc security
rebuild is recorded in [native-tool-security.md](native-tool-security.md).
Docker remains for disposable service dependencies and
explicit deployment rehearsals. PostgreSQL and the Testcontainers cleanup helper
are built locally from pinned sources; explicit builds may download the pinned
Linux base and verified build dependencies.

## Completed evidence

- The generation regression first failed against the previous Docker wrappers
  with a deliberately failing Docker executable. After conversion,
  `tooling/verify-native.sh` passed with Docker unavailable: SQL generation and
  vet, both actual protobuf trees, the administration browser bundle, and the
  generator fixture's Go, TypeScript and browser checks. Ownership, invalid
  protobuf, symlink, stale-output and drift checks also passed.
- `env PATH=/usr/bin:/bin make build` passed without Go or Node on the caller's
  PATH or mise shell activation. `make doctor` passed with the selected tools.
- The locally built PostgreSQL 18.6 image passed TCP and SQL readiness and
  checked removal. The local-image provenance check passed for PostgreSQL and
  the cleanup helper.
- `scripts/smoke.sh smtp` passed using the shared fixture runner: incoming mail,
  submission, queued relay, IMAP byte equality and two process rounds. The
  runner removed its PostgreSQL container before returning.
- The offline restore rehearsal passed using the generated member gRPC client:
  four rotations, two sealed reclamations, fixed targets and status/replay,
  source database and member-root destruction before target restore, original
  bytes, rejected invalid archives, and one outbound handoff.
- `make test-script-processes` passed the actual Python wrapper timeout and
  resistant-descendant regressions without Docker, including successful exits
  that leave quiet background children. It is part of the baseline.
- The schema rehearsal passed on three independent PostgreSQL clones: nine
  refused process startups, three refused check-only probes, no storage roots
  created, and unchanged database authority, schema definitions and physical
  object identities. The original installation still served and accepted mail.
- The complete fixture race suite passed after adding regressions for failed
  cleanup-helper startup and descendants surviving an ordinary command exit.

Parent command logs are under `.artifacts/native-tooling/`. These local artifacts
are execution evidence, not a substitute for committed regression tests.

## Integrated baseline and review

`make baseline-check` passed with unchanged source inputs. It includes strict
lint and formatting, root and fork vet, command builds, SQL/RPC/browser
generation checks, sqlc vet, separate fixture lint and race tests, Python
process regressions, root PostgreSQL tests with `-race -short -coverpkg=./...`,
all reviewed coverage floors, and both complete patched mail-library race suites.
The completion marker and matching source hashes are under
`.artifacts/baseline-check`; the combined log is
`.artifacts/native-tooling/final-baseline.log`.

The restore harness now uses the current member gRPC protocol. Python command
timeouts use process-group cleanup and bounded output draining.
The harness follow-up review found no remaining functional defects. Its coverage
rounding finding was corrected: the new helper's initial floor is 78.2%, below
the measured 108/138 statements (78.2609%); no existing floor changed.
Fixture follow-up review identified diagnostic logging consuming the removal
deadline. Diagnostics now have their own two-second timeout, followed by a fresh
30-second removal budget. Real-Docker regressions verify deletion and absence
despite stalled logs for both failed startup and failed child commands. The full
fixture race suite passed. A fresh narrow review confirmed the fix and found no
remaining defect in the reviewed paths.

## Limits

Execution evidence here is Linux amd64. The mise lock contains both Linux
architectures, but native arm64 execution remains a release gate. These checks
do not establish AWS availability, multi-zone durability, production throughput
or completion of the broader engineering-foundation security/release gates.
The root baseline uses `-short`; it does not rerun the large-message cases
recorded in the previous [RPC/complexity validation](sql-rpc-complexity.md).
Container and Kubernetes deployment rehearsals were adapted to local image
provenance and no-pull behavior, but were not rerun in this tooling slice.
