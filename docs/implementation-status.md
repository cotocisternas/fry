# Implementation status and evidence

FRY is a Linux prototype for business email/document exchange. This topic-based
index preserves the implementation and validation narrative previously collected
in the [README](../README.md). Linked records describe the conditions and limits
of their runs. A recorded passing result does not mean that a rehearsal was rerun
after every later change or establish production readiness.

For orientation, use the [code map](code-map.md),
[architecture brief](design/architecture-brief.md), [ADRs](adr/) and
[glossary](../GLOSSARY.md). For current commands and check selection, use
[development tooling](development.md) and [contributing](../CONTRIBUTING.md).

The [contributor quality pass](validation/contributor-quality.md) records the
shared-logic cleanup, behavioral regressions, lint-policy additions and local
verification performed before expanding collaboration.

The [2026-10-18 security audit](validation/security-audit-2026-10-18.md) records
MIT licensing, secret-scan triage and the original dependency/image advisories.
The [Go remediation record](validation/go-remediation-2026-10-18.md) documents
updated dependencies and subsequent scans. These point-in-time checks predate
the continuous security gate now included in `make check`; they do not establish
production readiness.
The [local repository preparation](validation/repository-initialization.md)
records canonical module paths, source-only setup verification, subsequent tool
and image remediation, and the remaining security limits.

The public repository lives at [cotocisternas/fry](https://github.com/cotocisternas/fry).
The [contributor workflow](development.md#ci-and-local-verification) runs native
quality, real PostgreSQL race/coverage, fork, browser, security and administration
API compatibility checks through `make check`. The [onboarding record](validation/contributor-onboarding.md)
and [browser runner record](validation/browser-ci.md) distinguish local evidence
from hosted execution. After the public-visibility change, the active
[main ruleset](https://github.com/cotocisternas/fry/rules/25366406) requires PRs
and passing `CI`, with no bypass actors. See
[repository protection](development.md#repository-protection).

## Current engineering status

The [hosted CI recovery record](validation/ci-recovery.md) identifies the
successful full validation and container-packaging run at `bcfd0e1`, including
the matching local checks and environment limits. It does
not extend earlier storage, migration or Kubernetes rehearsals to that revision.

| Area | Implemented and checked | Still pending |
| --- | --- | --- |
| Development and CI | Native mise tools, strict lint/format/modernization checks, centralized sqlc, generated RPC clients, PostgreSQL race/coverage tests, full mail-fork suites, browser and security gates; Linux amd64 hosted validation and packaging; PR-only main ruleset | Native arm64 execution and release qualification |
| Administration | Typed Connect HTTP/JSON and gRPC, UI/CLI clients, local/OIDC human sessions, scoped authorization and audited full downloads | Service accounts, request deduplication, resource revisions, durable jobs, resumable downloads, complete cursor/error contracts; see the [API gap list](reference/admin-rpc.md#unavailable-foundations-and-compatibility) |
| Observability | Optional process-owned OpenTelemetry traces/OTLP metrics and bounded instrumentation | Complete logging, readiness and operational metrics contracts; see [telemetry limits](design/sql-and-telemetry.md) |
| Deployment and recovery | Local storage/process/container validation records and an opt-in AWS S3 probe | Real AWS/IAM execution, metadata HA, automatic crash recovery/fencing, replica repair, AZ-loss evidence, representative capacity and cutover validation |
| Releases and security | Public repository, MIT license, source/security scans and explicit expiring exceptions | Maintainer-triggered release artifacts, SBOMs/provenance and native arm64 evidence; Kubernetes rehearsal remains on its [security hold](validation/local-kubernetes.md) |

The targets for no accepted-mail loss and recovery within five minutes after
an AZ failure, and a 30-minute migration cutover, remain requirements to prove.
Optional filtering, quarantine, document routing and application delivery are
later milestones under [ADR 0014](adr/0014-prove-conventional-email-first.md).
This status table records gaps, not a new implementation plan.

## Scope and remaining limits

**This is a Linux prototype, not a production distributed deployment.** Members can run
in-process or as separate `fry-member` processes over mutual TLS. HA metadata, automatic
crash fencing/recovery, replica repair, production-scale scheduling and complete IMAP
compatibility remain unimplemented or unvalidated. Public TLS transports and remote
reclamation are implemented and exercised locally, as described below. The schema and
on-disk format are experimental; use synthetic data only. Shared NFS storage is not
supported. S3 requests are tested against a local HTTP fixture; real AWS and IAM
validation are outstanding.

## Receipt storage and lifetime

- [`internal/hotstore`](../internal/hotstore): bounded streaming, checksummed append
  records, fsync, verified range reads, and conservative recovery.
- [`internal/receiptstore`](../internal/receiptstore): PostgreSQL attempt/receipt
  authority, streaming to two local or authenticated remote members, atomic
  destination/reference/work publication, retry/status resolution, and
  abort/cancellation fencing.
- Packed-object construction/publication and verified cold reads, backed by
  [`internal/objectstore`](../internal/objectstore): an AWS S3 adapter with conditional
  uploads and permanent retirement markers for unpublished packs.
- Reference lifetime, bounded read handles, pack compaction, hot-file reclamation,
  scratch recovery, and cleanup deadline accounting.

Read the [local store evidence](validation/local-hotstore.md), [replicated receipt
evidence](validation/replicated-receipts.md), [packed-object
evidence](validation/packed-objects.md), [storage lifecycle
evidence](validation/storage-lifecycle.md), [SMTP reception
evidence](validation/smtp-reception.md), [mailbox/IMAP
evidence](validation/mailbox-imap.md), and [collection
evidence](validation/imap-collection.md) for the implemented contracts and limits. A
local hotstore locator alone must never authorize SMTP success; receiptstore owns the
two-copy-plus-metadata acceptance sequence. Deployment durability still depends on
actual independent failure domains and appropriately durable metadata.

The coordinator also supports [scheduled packing and cleanup with remote hot
reclamation](validation/storage-maintenance.md). The process smoke verifies cold reads
after hot-copy removal and hot acceptance during a simulated object-store outage; actual
AWS and capacity targets remain unverified.

[Cleanup
scans](validation/storage-maintenance.md#cleanup-progress-around-blocked-receipts)
advance around blocked receipts and revisit older debt without changing the checks
required before recording physical removal. This does not establish the production
cleanup deadline.

## Mail protocols, collection and migration

- [`internal/outbound`](../internal/outbound): durable recipient queues, authenticated
  relay handoff, bounded retries and failure notices; local authenticated submission
  uses the SMTP adapter.
- [`internal/messageindex`](../internal/messageindex): bounded MIME metadata and exact
  encoded attachment spans.
- [`internal/replicastream`](../internal/replicastream): verified stream completion for
  [mutually authenticated replica transport](validation/replica-transport.md), with
  generation-scoped writes and member-owned read protection.
- [`internal/imapcollect`](../internal/imapcollect): external IMAP collection, folder
  mapping, source deduplication, and optional UID-targeted cleanup.
- PostgreSQL mailbox publication, stable UIDs, flags, atomic APPEND, bounded MIME
  structure/attachment access, decoded-header search, identity-safe folder operations,
  subtree/INBOX rename, persistent subscription names, COPY/MOVE, and
  [`internal/imapserver`](../internal/imapserver) for a tested IMAP subset.
- [`internal/smtpserver`](../internal/smtpserver) and [`cmd/fry`](../cmd/fry): loopback
  SMTP reception with durable acceptance.
- [`internal/maildirectory`](../internal/maildirectory): persistent incoming domains,
  aliases/catch-alls, and IMAP credentials, with live protocol updates.
- [`internal/admin`](../internal/admin) and [`cmd/fry-admin`](../cmd/fry-admin): scoped
  administrator accounts, web/HTTP/CLI management, audited raw downloads, and generic
  OIDC claim provisioning.

History migration has a separate staged importer and operator CLI. It supports bounded
background passes, restart-safe copy attempts, exact final mailbox reconciliation, and
generation-fenced activation. See [migration validation and
commands](validation/mailbox-migration.md). The 30-minute cutover target still needs
representative-scale rehearsal.

Explicit public mail transports support incoming SMTP STARTTLS, protected submission on
587 or 465, IMAP 993, and direct administration HTTPS. See [transport validation and
limits](validation/public-mail-transport.md) and the [public configuration
example](../examples/public-mail.json); production deployment gates remain unverified.

Public submission also requires [outgoing sender
authority](validation/outgoing-sender-authority.md): organization-scoped DNS controller
proof or an audited global override, with revocation fencing new acceptance.

SMTP credentials can be [created and managed live](validation/submission-credentials.md)
through the web interface, HTTP and CLI. PostgreSQL owns their sender restrictions,
passwords, enabled state and generations; startup entries seed missing accounts only.
Changes fence new acceptance in existing sessions, while accepted mail remains queued.
Relay selection stays in deployment configuration, and public submission also requires
an organization sender grant. Run `./scripts/smoke.sh submission-credentials` for the
disposable process/restart rehearsal.

[Pre-read SMTP capacity deferral](validation/smtp-capacity-deferral.md) preserves a
reusable `451` response after confirmed nonacceptance and bounded DATA framing
completion. Focused/full race tests and the repaired process rehearsal pass, including
reusable temporary replies during capacity exhaustion.

## Bounded hot files, capacity and diagnostics

[Member-file catalog validation](validation/member-file-catalog.md) records exact file
ownership and rewrite cleanup checks. The subsequent bounded current/sealed-file
implementation uses this authority; retained-data scale, concurrent maintenance and
production recovery remain separate gates.

[Bounded hot-file admission](validation/bounded-hot-admission.md) adds opt-in
record/byte ceilings and durable per-target capacity reservations. Correctness and
restore checks pass. [Capacity lanes](validation/hot-capacity-lanes.md) improve local
intake by partitioning reservation updates, with a documented fragmentation tradeoff;
[conditional automatic rotation](validation/automatic-hot-rotation.md) is available as
opt-in scheduled maintenance.

[Explicit hot-file rotation](validation/hot-file-rotation.md) seals a bounded current
file while preserving historical message locations, with local and authenticated remote
reads through bounded read-only descriptors. Full race tests and fresh restore across
sealed/current files pass. [Sealed-file
reclamation](validation/sealed-file-reclamation.md) preserves survivors and the active
writer while removing expired physical copies; full race tests and fresh restore after
actual compaction pass. [Automatic rotation](validation/automatic-hot-rotation.md)
resumes intake after bounded-file exhaustion in a real-process rehearsal. Its earlier
capacity-rejection disconnects were repaired by [bounded SMTP
deferral](validation/smtp-capacity-deferral.md); incomplete framing and ambiguous
acceptance still require conservative disconnection.

[Current-file closure-proof validation](validation/current-reclaim-close-proof.md)
covers reclamation and shutdown faults, retaining ownership and cleanup debt when
descriptor closure is uncertain. Focused and full PostgreSQL race tests and vet pass.

[Member-storage diagnostics](validation/member-storage-diagnostics.md) give global
operators bounded catalog, reservation and owner-state observations through HTTP, CLI
and the global-operator web view. Focused/full race tests and the process rehearsal
pass. Charges are not filesystem usage or free-space measurements.

## Accounting and local performance experiments

[Advisory storage quotas](validation/storage-quotas.md) expose exact logical
retained-original usage, scoped thresholds and durable local alert transitions through
the Storage tab, API and CLI. Thresholds never gate mail acceptance. Run
`./scripts/smoke.sh storage-quotas` for the disposable rehearsal; physical disk
accounting, external notifications and production scaling remain separate work.

Use the [concentrated-inbox load harness](validation/local-load.md) to reproduce bounded
SMTP/IMAP measurements on synthetic data. [Static ingestion
shards](validation/static-ingestion-shards.md) provide independent replica pairs with
durable fixed placement. The [storage scaling
design](design/storage-scaling-next-step.md) describes the remaining metadata and
segmented-reclamation work.

Mailbox publication uses [rotating organization turns](validation/mailbox-fairness.md)
so one organization’s backlog does not take every publication batch. PostgreSQL
query-plan checks cover the indexed selection path; outbound delivery and cleanup
scheduling retain separate fairness limits.

A [publication experiment](validation/mailbox-publication.md) compared individual
transactions with bounded groups. Grouping improved visibility but harmed or
destabilized intake latency, so individual publication transactions remain in use; the
tested candidate is preserved as an unapplied patch.

[Acceptance profiles](validation/acceptance-profile.md) identified exclusive
inbox-admission locking as a concentrated-inbox bottleneck. Ordinary deliveries share
that gate; migration reservation remains exclusive, with preparation and final
acceptance still fenced.

[Bounded header capture](validation/header-capture.md) reuses the incoming stream and
existing receipt transaction to reduce later IMAP reads. Captured metadata must match
both durable originals; malformed, oversized or budget-skipped headers retain the
lazy-read path. This is an optional optimization, not a filter or a guarantee that every
message has precomputed metadata.

[Retained hot-file measurements](validation/retained-storage.md) cover warm-cache
Open/Inventory up to 65,536 records and actual half-expired whole-member reclamation up
to 4,096 accepted originals. Every retained original was verified; these local results
identify record-count costs without establishing production capacity or a segmented
implementation.

## Schema installation

[Schema initialization](design/schema-installation.md) commits receipt, administration
and mail-directory definitions together with a version ledger. `fry -config CONFIG
-check-schema` checks compatibility without opening storage. All database-owning
commands reject unversioned or incompatible installations before storage startup; no
conversion of existing prototype databases is supplied. The [validation
record](validation/schema-installation.md) tracks the PostgreSQL tests and process
checks. Keep existing prototype data with its existing binaries until a supported
conversion is available.

## Deployment and restore rehearsals

Build the local container image with `make container IMAGE=fry:local-test`, then run
`./scripts/smoke.sh containers` for a disposable TLS mail/restart rehearsal. [Container
runtime evidence](validation/container-runtime.md) covers nonroot operation, read-only
roots, retained member volumes and conservative crash fencing. This does not provision
AWS.

The [Kubernetes rehearsal](validation/local-kubernetes.md) is currently
security-held: `./scripts/smoke.sh kubernetes fry:local-test` refuses execution
before creating resources. Its historical run checked pod permissions,
authenticated storage, exact mail retrieval and clean replacement on retained
PVCs. Production EKS and availability-zone recovery remain separate gates.

Run `./scripts/smoke.sh offline-restore` for the [offline hot-only restore
rehearsal](validation/offline-restore.md): a stopped synthetic installation's full
database and both member roots are restored into a fresh authority, with independent
replica, mailbox, account-policy and queued-handoff checks.
`./scripts/smoke.sh cold-restore` adds a [cold-pack restore
rehearsal](validation/cold-restore.md) with full scratch and exact object metadata,
including retired markers, at the same local fixture namespace. Both passed; real AWS
recovery, online backup, trusted old-deployment fencing and AZ recovery remain separate
gates. Cloned database locks do not fence the original installation.

The [isolated AWS S3 validation bundle](../deploy/aws/README.md) includes a
dedicated-bucket/two-role template and an opt-in contract runner. Its local checks pass;
[real AWS execution remains pending](validation/aws-s3-bundle.md) an account and
temporary-role access. It provisions no resources automatically and provides no EKS,
AZ-recovery or production-capacity evidence.

## Engineering and native tooling

Use the mise-managed native toolchain on Linux. Run `make setup` to install pinned tools
and pull digest-pinned upstream test dependency images, then `make doctor`. See [development
tooling](development.md). Go 1.27.2 matches the root module requirement and runtime
image build. The PostgreSQL and S3 adapters use pinned pgx v5 and AWS SDK for Go v2
dependencies.

Toolchain updates should select a verified stable release, update these pins together,
and pass the root integration suite and both patched email-library suites. The nested
library modules keep separate Go declarations (the IMAP fork's minimum was
raised during dependency remediation); run their tests with the same pinned
compiler as FRY.

The agreed [engineering foundation](design/engineering-foundation.md) prioritizes code
quality, linting and meaningful test coverage before release automation. Its broader
implementation remains in progress. The [SQL and telemetry
foundation](design/sql-and-telemetry.md) adds sqlc-generated PostgreSQL queries and
optional OpenTelemetry traces and metrics. Run `make sqlc-check sqlc-vet` to validate
generated queries, and `make sqlc` after editing SQL.

The lightweight `make test` may skip PostgreSQL cases. Required integration runs use
FRY-owned Testcontainers-Go fixtures and digest-pinned upstream PostgreSQL/cleanup
images; missing local images fail with instructions to run the explicit pull target. See the
[contribution guide](../CONTRIBUTING.md#choose-checks-for-the-change) for focused commands.

The runner binds PostgreSQL to a random loopback port and removes its container/storage
and scratch directory afterward. It does not use AWS or a production database. The full
suite includes a streamed 200 MiB local record; `-short` omits that case. Tests also
terminate isolated storage processes and a test database connection at selected failure
points.

[SQL/RPC/complexity validation](validation/sql-rpc-complexity.md) records the
centralized SQL inputs, typed Connect administration clients, member gRPC and
strict handwritten-code baseline. Use the current
[administration](reference/admin-rpc.md) and [member](reference/member-rpc.md)
references to distinguish implemented behavior from broader API designs.
[Native tooling validation](validation/native-tooling.md) identifies the checks
and rehearsals rerun in the tooling phase. The
[Go quality policy](research/go-quality-policy.md) and
[lint gap audit](research/lint-gap-audit.md) explain analyzer selection and gaps.
These local records precede the CI validation suite described above. Release
qualification and production validation remain separate requirements.

## Run a local mail process

Complete [native setup](development.md#setup) first. These examples use synthetic
data and are not production installation instructions.

Use a fresh, dedicated PostgreSQL database and set `FRY_DATABASE_URL` locally. The
example accepts synthetic mail for `invoices@example.test` and `billing@example.test`
into one destination. Set `FRY_LOCAL_IMAP_PASSWORD` locally for the example account
`invoices`. The first launch atomically initializes the versioned prototype schema:

```sh
./scripts/toolchain.sh go run ./cmd/fry -config examples/local-smtp.json -init
```

Subsequent launches omit `-init`. Listeners are restricted to loopback; the example uses
SMTP port 2525 and IMAP port 1143. Accepted mail is published into INBOX by a background
worker. Its two local storage directories do not provide independent failure domains.
For a fully disposable process/restart check, run `./scripts/smoke.sh smtp` with Docker
available.

For the complete local mail loop, use [local-mail.json](../examples/local-mail.json) and
also set `FRY_LOCAL_SMTP_PASSWORD`. It adds authenticated submission on port 2587; its
synthetic relay points back to the local incoming SMTP listener. See [outbound
validation and limits](validation/outbound-relay.md).

### Separate storage processes

Run `./scripts/smoke.sh replicas` with Docker, OpenSSL, and Python available. It
generates a disposable private CA, starts two `fry-member` executables and one
coordinator, and checks exact SMTP/IMAP bytes, clean restart, abrupt member death,
surviving-copy reads, and rejection of unsafe takeover. Everything runs on localhost
with synthetic data. See [replica operation and limits](validation/replica-transport.md)
for configuration and remaining recovery gates.

### Administration demo

Run `make demo` with Docker available. It starts a disposable localhost
installation with synthetic mail and prints its fixture credentials. Ctrl-C removes its
data. The interface manages incoming domains, aliases, inboxes, IMAP credentials,
administrator assignments, and audited downloads. The same operations are available
through `./scripts/toolchain.sh go run ./cmd/fry-admin -help`. See [administration
validation and limits](validation/administration.md), including generic OIDC support and
remaining production gates.
