# FRY — architecture discovery brief

The architecture-level interview is complete. This brief records the agreed direction and identifies implementation proposals and remaining evidence. It is not an implementation specification, resource sizing result, or validated recovery guarantee. Detailed decisions remain in [discovery](discovery.md) and the [ADRs](../adr/).

## Product and first milestone

Build FRY in Go for business email/document exchange, initially preserving conventional email behavior for existing applications. Include direct SMTP receipt, independent-copy collection from configurable external IMAP folders, shared-mailbox IMAP access, and durable outgoing queues using the default authenticated relay or an organization's own MTA. Organizations have isolated domains, inboxes, and permissions; aliases and optional domain catch-alls are supported.

The first milestone includes an administration web interface, API, and CLI, local administrator accounts, and one generic OIDC provider per installation. Internal organization managers are restricted to assigned organizations; global operators administer the installation. Privileged email-content access is permissioned and audited. Import existing history, folders, dates, and flags; a coordinated final cutover may pause service for at most 30 minutes, with client resynchronization continuing afterward.

Filtering starts disabled. Spam/malware/document filtering, routing, independent application views, and API/webhook application delivery belong to later milestones; their agreed behavior is retained in discovery.

## Storage direction

Implement a simpler FRY-owned version of the shard-and-pack pattern: distribute receipts, durably replicate hot bytes, then asynchronously combine messages into immutable object-store packs. SCRAP is reference material only, not a dependency or an adaptation target. Final SMTP success requires durable original bytes and recoverable receipt/destination/work state. Live mailbox references remain independent of recovery-retention expiry.

The [current storage proposal](replicated-hot-storage.md) uses PostgreSQL as the metadata authority and proposes two distinct-AZ hot copies, bounded append storage, SQL-backed work, packed S3 objects, and range reads. The [detailed protocol](storage-protocol.md) now specifies the proposed contracts, including attempt registration, publication fencing, read pins, and physical cleanup. These remain implementation proposals requiring infrastructure validation. The earlier [S3-first architecture](proposed-architecture.md) and its diagram remain historical comparison baselines.

## Engineering evidence

The first [local Go hot-byte-store prototype](../validation/local-hotstore.md) implements bounded staging, checksummed durable append, range reads, and conservative restart/tail reconciliation. File I/O, subprocess-exit, corruption, concurrency/race, and a synthetic 200 MiB stream test pass.

The second [receipt implementation stage](../validation/replicated-receipts.md) adds real PostgreSQL attempt/receipt state, two in-process local members, atomic receipt/destination/reference/work publication, retry/status resolution, and abort/cancellation fencing. Disposable PostgreSQL integration and race tests pass. The third [packed-object stage](../validation/packed-objects.md) adds conditional S3 uploads, PostgreSQL publication guards, verified cold reads and retirement of unpublished objects. Its SDK requests use a local HTTP fixture; real AWS/IAM validation remains outstanding. The fourth [lifecycle stage](../validation/storage-lifecycle.md) implements references, bounded read ownership, pack compaction, local hot-file replacement, scratch recovery and cleanup deadline accounting. It pauses a local member for a rewrite and retains unresolved readers as debt. The subsequent local mail stages add [SMTP reception](../validation/smtp-reception.md), durable [shared mailbox/IMAP state](../validation/mailbox-imap.md), and [external IMAP collection](../validation/imap-collection.md). A runnable command and disposable process/restart smoke exercise SMTP-to-IMAP delivery, authenticated submission, and [queued outbound relay handoff](../validation/outbound-relay.md). The [administration stage](../validation/administration.md) adds persistent administrator accounts, scoped permissions, audited downloads, web/HTTP/CLI management, generic OIDC, and live incoming-domain/alias/IMAP-credential configuration. The [replica transport stage](../validation/replica-transport.md) connects standalone storage members over mutual TLS with generation-scoped publication, verified stream completion, and member-owned read pins. A separate-process smoke verifies clean restart and conservative behavior after abrupt member death. The [history migration stage](../validation/mailbox-migration.md) adds isolated staging, bounded background/resume passes, final flag/folder/presence reconciliation, source preservation, and an operator CLI with explicit barrier attestation and generation-fenced activation. Real IMAP sessions, PostgreSQL failure tests, and the separate-process CLI exercise these contracts. The [running lifecycle stage](../validation/storage-maintenance.md) adds bounded scheduling and authenticated remote hot reclamation, including reads after cold packing and hot buffering during a simulated object outage. The [public transport stage](../validation/public-mail-transport.md) adds explicit SMTP STARTTLS and implicit TLS, IMAP TLS, and direct administration HTTPS, with bounded handshakes and conservative shutdown. Immutable object namespace binding prevents a configuration change from silently redirecting pack reads or cleanup. Immutable relay IDs likewise preserve the selected downstream destination. The [sender authority stage](../validation/outgoing-sender-authority.md) adds organization-scoped DNS challenges, recorded global overrides, web/API/CLI management, and revocation checks at durable SMTP acceptance. The [static ingestion catalog](../validation/static-ingestion-shards.md) now distributes attempts across independent replica pairs and preserves target authority through retries and reclamation. [Concentrated-inbox measurements](../validation/local-load.md) now cover 5,000 messages: persisted receipt-governed header metadata speeds repeated searches, and [bounded streaming capture](../validation/header-capture.md) reduces first-query extraction work while retaining lazy fallback. Initial header extraction and acceptance throughput still need production-scale validation. [Acceptance profiling](../validation/acceptance-profile.md) removed an exclusive inbox-admission bottleneck while preserving migration fencing. A [publication experiment](../validation/mailbox-publication.md) improved visibility but harmed or destabilized intake latency, so transaction batching was not adopted. Same-filesystem one/two/four-pair runs demonstrate distribution without establishing a throughput gain. [Persistent SMTP credentials](../validation/submission-credentials.md) now support live rotation and disablement through the web interface, HTTP and CLI. [Organization turns](../validation/mailbox-fairness.md) improve mailbox-publication opportunity fairness with independently checked SQL plans. Local [container](../validation/container-runtime.md) and [Kubernetes](../validation/local-kubernetes.md) rehearsals exercise packaging and clean storage replacement. HA metadata, automatic crash fencing/recovery, production-scale scheduling and retained-data validation, complete protocol compatibility, representative-scale cutover timing, and AZ-loss validation remain outstanding.

The [interactive architecture and cost comparison](storage-architecture.html) brings together the current design, revised pricing, and bounded failure-model results. The [executable state model and validation matrix](../validation/storage-failure-validation.md) checked 1,456 states and 3,882 transitions under explicit assumptions and detected counterexamples when three safeguards were removed. This is specification evidence, not a filesystem, PostgreSQL, S3, throughput, or recovery test.

The [revised packed-storage cost estimate](../research/packed-storage-cost-estimate.md) models approximately USD 30,525/month against the earlier USD 29,506/month baseline at hypothetical 20M messages/day, 250 decimal KB/message, 30 stored days, and 50% external downloads. Lower PUT charges do not establish lower total cost: hot copies, workers, replication traffic, and cleanup add costs. Both subtotals retain unbenchmarked infrastructure and important exclusions.

SMTP credential creation and sender-policy editing are now live through the
web interface, HTTP and CLI. The database is authoritative for organization-owned
sender restrictions, password, enabled state and generation; bootstrap is
insert-only. Relay selection remains deployment configuration, and public
submission also checks organization sender authority. See the
[credential contract and validation status](../validation/submission-credentials.md).
The integrated process rehearsal passed seven exact handoffs and four clean
restarts; current local Chromium browser checks, the full PostgreSQL race suite
and vet also passed. Earlier rotate/disable browser evidence remains historical.

The [offline hot-only restore rehearsal](../validation/offline-restore.md) passed
with a full stopped database/member backup, five originals independently verified
through each replica, preserved IMAP identity/flags/dates and account policy, one
restored queued handoff and one new acceptance. This is clean local synthetic
restore evidence. The [cold-pack follow-up](../validation/cold-restore.md) also
passed with restored scratch and exact object metadata at the same local fixture
namespace, including retired markers and reads with both hot members stopped.
Actual AWS recovery, online/crash recovery, trusted fencing of the old installation,
authorization/deletion rollback policy and AZ recovery remain separate gates;
cloned database locks are not deployment fencing.

[Advisory storage accounting and local alerts](../validation/storage-quotas.md)
are implemented: each retained accepted original counts once until physical
removal, including retiring debt, with scoped administration and no admission
gate. Component/full race, process and browser checks passed. Physical disk/S3
metrics, external notifications and production-scale behavior remain separate
validation and design work; short local measurements do not select capacity.

[Retained-storage measurements](../validation/retained-storage.md) now quantify
warm-cache whole-file Open/Inventory up to 65,536 records and receipt-governed
whole-member rewriting up to 4,096 accepted originals. Record-count memory and
whole-call rewrite costs are visible in the local results; production sizing,
peak memory, cold restart and concurrent maintenance of bounded current/sealed files remain separate validation gates.

## Operational decisions

- **Initial deployment:** AWS São Paulo, one region across multiple AZs, Kubernetes preferred. Target no accepted-mail loss and restored reception/mailbox access within five minutes of one AZ failing. EKS, zone count, node sizes, and replica topology remain proposals. Later independent installations may be on-premises, single-site, or multi-region with explicit guarantees and residency boundaries.
- **Reduced protection:** automatically continue acceptance with alerts once safe ownership and healthy surviving durable storage are established, without an elapsed-time cutoff. An unavailable authoritative database or unsafe physical capacity still prevents acceptance.
- **S3 upload outage:** size local storage for six hours of incoming mail at the expected byte rate, configurable, while replicas and metadata storage remain healthy. This is a capacity target, not a retention period or a timer that stops acceptance. Safe-capacity exhaustion temporarily defers SMTP receipt and pauses IMAP collection.
- **Cleanup:** once no mailbox, pending work/delivery, or retention/grace reference requires an email, physically remove its bytes from active storage within 24 hours by default, configurable. Packed-object compaction may be necessary. Backup/snapshot erasure was not included in this answer.
- **Organization quotas:** advisory thresholds trigger administrator alerts; continue new incoming mail and collection while shared capacity is safe. Do not delete live mail or reject solely because an organization crossed its advisory quota. Preserve fair service across organizations.

## Remaining work

### Documentation readiness audit

The architecture interview is complete, and the existing 22 ADRs capture the agreed consequential choices, including fixed ingestion placement, bounded current/sealed file storage and an explicit schema installation contract. There is no missing ADR that blocks the first storage prototype. Do not turn provisional choices such as PostgreSQL/RDS, EKS, three AZs, shard counts, or a particular IMAP library into accepted decisions merely because they appear in the reference design. Record later technology commitments when their trade-offs have been evaluated.

| Area | Current record | Next gate |
| --- | --- | --- |
| Product scope and decisions | [Discovery](discovery.md), [ADRs 0001–0022](../adr/), [glossary](../../GLOSSARY.md) | Preserve conventional-email-first scope; refine individual features when implemented. |
| Receipt storage and lifecycle | [Storage protocol](storage-protocol.md), [running maintenance](../validation/storage-maintenance.md), [replica transport](../validation/replica-transport.md) | Validate real object-store/IAM behavior, metadata HA, trusted crash fencing, replica repair, bounded current/sealed storage and concurrent maintenance at retained scale, and failure recovery at representative load. |
| SMTP/IMAP and collection | [Mailbox state](../validation/mailbox-imap.md), [collection](../validation/imap-collection.md), [outbound](../validation/outbound-relay.md), [TLS transport](../validation/public-mail-transport.md) | Test real integration clients and concentrated-inbox load; complete relay administration, throughput scheduling, and deployment abuse controls. |
| Administration and access | [Implemented admin/OIDC scope](../validation/administration.md); ADRs 0004 and 0014 | Complete relay administration, filesystem/S3 usage metrics, external alert delivery, retained-scale validation and production account protections. |
| Migration and operation | [Staged migration and CLI](../validation/mailbox-migration.md); ADRs 0002, 0009, 0010, 0014 | Measure final cutover against the 30-minute target; extend the passed hot/cold offline restore rehearsals to actual AWS, trusted old-deployment fencing and online consistency; add repair runbooks, versioned schema upgrades and AZ-loss acceptance tests. |
| Capacity and cost | [Revised cost model](../research/packed-storage-cost-estimate.md) | Measure traffic, retained data, client behavior, and fault recovery; replace illustrative sizing. |
| Later optional features | ADRs 0006, 0011–0014; discovery backlog | Specify filtering, quarantine, routing, and application delivery before their milestones. |

No further general interview is required to continue implementation and local validation. Remaining questions should be asked at their implementation or rollout gate, not as another general questionnaire. Before production, explicitly settle backup/snapshot retention and erasure expectations, recovery/audit retention and outbound retry defaults, and the supported message-size limit. Operational measurements and protocol compatibility are evidence to gather, not estimates to repeatedly request from the user. Detailed permission and lifecycle proposals should be made concrete for review before asking for decisions that change access or data-retention policy.

The documents are sufficient for architecture handoff and the first prototype; they are not a complete implementation specification for every first-milestone feature or evidence of production readiness.

Measure incoming byte rates and burst duration, retained mailbox bytes, size distribution, external downloads, and real IMAP-client behavior. Reported tens of millions of messages/day, a possible 500,000-message batch into one inbox, and a tentative 200 MB maximum example are not sufficient to size production. Do not repeat requests for estimates the user already identified as unknown.

Implement and fault-test the specified append/replication acknowledgments, database publication, fencing, retries, recovery, reads, compaction, and admission contracts. Follow the staged implementation gates in the detailed protocol and validation matrix. Validate the selected IMAP implementation with real clients, mailbox concurrency, a concentrated inbox batch, retained-data scale, migration, and AZ failure. Replace cost assumptions with those measurements; previous one-object-per-message figures remain a comparison baseline.

Detailed administrative/API workflows, physical quota metrics, external alert delivery, retention/retry defaults, backup erasure, and later optional-feature policies remain explicit follow-on specification work. Closing this interview does not silently settle those policies or authorize provisioning.

The implemented [member-file catalog](../validation/member-file-catalog.md), recorded in [ADR 0017](../adr/0017-track-hot-file-lifecycles-explicitly.md), strengthens file ownership and rewrite deletion authority for the later bounded current/sealed stages; it does not authorize crash takeover.

The [bounded-admission prototype](../validation/bounded-hot-admission.md) reserves capacity before dispatch and preserves uncertain-write accounting. Its measurements exposed shared file-counter contention; the subsequent [capacity-lane implementation](../validation/hot-capacity-lanes.md) improves local refund throughput while retaining hard aggregate ceilings and accepting conservative partition fragmentation. The bounded current/sealed stages described below extend this work; retained-scale validation, concurrent maintenance and distributed recovery remain separate gates.

The [explicit rotation slice](../validation/hot-file-rotation.md) preserves historical FileIDs and locators, provides bounded sealed-file reads, and checks spare rewrite capacity. Local/remote failure tests and a process restore with four rotations pass. The [sealed-file reclamation follow-up](../validation/sealed-file-reclamation.md) adds bounded source inventory, survivor replacement and explicit delete-only recovery without switching the current writer. Full race tests and fresh restore after reclamation pass. [Conditional automatic rotation](../validation/automatic-hot-rotation.md) is now opt-in, with a real-process four-record-file rehearsal verifying resumed intake and both independent originals. Concurrent maintenance, production scheduling guarantees and cloud failure validation remain outstanding.

The implemented [member-storage diagnostics](../validation/member-storage-diagnostics.md) provide global-only bounded catalog/reservation snapshots and nonblocking owner observations through HTTP, CLI and the global-operator web view. Focused/full race tests and the process rehearsal pass. Filesystem/S3 usage, free space and aggregate physical metrics remain future work; conservative charges are not measured disk consumption.

[Atomic schema installation and startup compatibility](schema-installation.md)
now give the prototype an explicit version boundary. Fresh initialization commits
all components together; incompatible or unversioned installations are refused
before opening storage. Focused PostgreSQL tests and the real startup-refusal
rehearsal passed. Registered upgrades, conversion of older prototypes and rolling
version compatibility remain future work; see [validation](../validation/schema-installation.md).
