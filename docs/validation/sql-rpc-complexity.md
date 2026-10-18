# Central SQL, RPC transport and strict complexity baseline

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Status: passed, 2026-10-18. The complete existing-code gate passed with unchanged
source inputs after the RPC review fixes. This record covers centralized SQL,
implemented RPC transports and the strict complexity baseline; it does not claim
completion of the broader engineering-foundation design. The historical baseline
is recorded in [engineering-baseline.md](engineering-baseline.md).

## Implemented changes

The root `sqlc.yaml` reads schemas in `db/schema` and queries in `db/queries`.
Installation embeds those same schemas through package `fry/db`. Generated query
packages remain private to their owning domain packages. There are no parallel
runtime schema copies or per-package generator configurations.

The administration contract contains 34 typed operations, with generated Go and
TypeScript clients. Connect-Go serves the RPCs; the CLI and browser use generated
clients. Existing unversioned JSON adapters remain for prototype compatibility.
Original-email downloads remain authenticated binary HTTP operations. See the
[administration reference](../reference/admin-rpc.md) for authentication,
permissions, routes and explicitly unavailable future mechanisms.

Member operations use protobuf gRPC over HTTP/2 with mutual TLS, pinned peer keys
and matching build identities. Database authority still determines acceptance,
ownership and maintenance outcomes. Payloads use bounded chunks and require size,
digest and stream-completion checks. See the [member reference](../reference/member-rpc.md)
for cancellation, durable closure and release identity requirements.

The stricter lint policy applies to production code and tests: cyclomatic
complexity 10, cognitive reporting threshold 15, and function limits of 60 lines
and 40 statements. It also adds nil/error consistency, exhaustive switches,
context and logging checks, unused-parameter checks and transport-to-persistence
import restrictions. Existing error, security and resource checks remain enabled.
Generated code alone uses the strict generated-file exemption; test files do not
have a blanket exemption. Refactoring preserves transaction ownership and test
assertions through named validation, lifecycle and scenario helpers.

`make generate-check` checks centralized SQL, both protobuf contracts, generated
clients and the browser bundle without rewriting them. `make proto-drift-check`
checks schema lint, compilation and RPC/browser reproducibility. These checks do
not establish compatibility with a previously released API major.

## Focused verification

- Administration RPC, domain and CLI PostgreSQL race suites passed, including
  standard `grpc-go` calls, current authorization, revocation, malformed requests
  and audit rollback. Both existing Chromium administration scripts passed using
  the RPC-backed browser.
- Member regressions passed for gRPC interoperability, peer/version/protocol
  rejection, malformed streams, lost replies, cancellation and read closure.
- The unchanged 200 MiB streaming test passed under race with 61,510,512 bytes of
  cumulative allocation against its existing 100 MiB limit. This fixture is
  compressible: bounded per-chunk DEFLATE reduces its transport work. That
  cumulative limit is **not** established for incompressible 200 MiB originals.
  Raw fallback and a generated incompressible round trip are tested separately.
- Command tests cover actual member startup and signal shutdown, and schema
  refusal before creating storage. The subprocess fixture now explicitly carries
  its isolated PostgreSQL schema in the DSN.
- Both complete patched email-library race suites passed. Their sources are
  unchanged by this transport/complexity phase.
- A disposable 32-message SMTP-to-IMAP run accepted all messages, verified all
  headers and checked eight full/partial payload samples. This is regression
  evidence, not production throughput or scale evidence.

The final `scripts/tests/baseline.sh` run passed with Go 1.27.2 and PostgreSQL
18.6 on Linux amd64. It checked strict lint, formatting, root and fork vet,
command builds, SQL/RPC/browser generation, sqlc vet, root tests with
`-race -short -coverpkg=./...`, every reviewed coverage floor and both complete
patched email-library race suites. Before/after source hashes matched.
The completion marker, source hashes and coverage reports are under
`.artifacts/baseline-check`; the combined log is
`.artifacts/strict-lint/final-baseline.log`.

Separate runs passed the 200 MiB IMAP streaming and attachment-indexing tests
omitted by `-short`. All four commands cross-compiled for Linux arm64. The Linux
amd64 container image built, and all four packaged commands passed help-command
smoke checks in read-only containers without network access. Cross-compilation
does not establish native arm64 runtime correctness; these container checks do
not establish deployment readiness.

Reviewed floor increases retain directly exercised behavior: admin CLI 82.3%,
member command 70.3%, installation 100%, replica configuration 86.5%, IMAP source
78.2%, and load verification 13.9%. New `fry/db` is 3/3 statements (100%); new
`admintransport` is 624/783 (79.693%, rounded down to a 79.6% floor). Generated-only
protobuf packages have explicit zero handwritten floors and still undergo
generation, build and integration checks. No existing floor was lowered.

Independent reviews of migration, packing, compaction, hot-store cleanup and
sealed reclamation found no introduced defect in their scoped comparisons.
RPC review identified two required fixes: preserving read-close uncertainty
across joined errors, and checking pooled TLS certificate validity before request
transmission. Both were fixed with regressions. A fresh independent review
confirmed both fixes and found no material new defect; its focused race rerun
covered actual-wire status, pooled expiry, exact EOF handling and compression
fallback. The final complete gate includes those fixes.

## Remaining engineering gates

This phase does not complete the entire engineering-foundation design. Scoped
service accounts, administrative request deduplication, resource revisions,
durable administrative jobs and cancellation, resumable binary downloads,
released-API compatibility enforcement, complete security/CI/release workflows,
native arm64 execution and production HA/scale validation remain separate work.
No AWS resources or live AWS validation are included.
