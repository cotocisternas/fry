# Engineering module design

Implementation was authorized on 2026-10-18. This document records accepted
requirements, including work not yet implemented. Use the
[current implementation status](../implementation-status.md#current-engineering-status)
and [administration API gap list](../reference/admin-rpc.md#unavailable-foundations-and-compatibility)
to distinguish those requirements from current behavior.

Status: accepted as part of the consolidated design, 2026-10-18.
Implementation is authorized. Numerical limits require validation.
This design refines [the foundation](engineering-foundation.md), preserves its
accepted constraints and follows [the contract](api-contract.md) and
[quality policy](../research/go-quality-policy.md). No code or schema changes
are part of this document.

## Original starting point and current layout

At design time, `internal/admin` combined application operations, database queries,
HTTP routing, OIDC and embedded HTML/JavaScript. `cmd/fry` also owns listener,
member transport and worker orchestration. `receiptstore` owns substantial durable
mail behavior, while `maildirectory` owns routing and mail credentials. These
were useful existing modules to deepen, not a reason to create a layer per table.
The existing browser is embedded vanilla HTML/CSS/JavaScript; no SPA framework
has been selected. sqlc output already exists in three module-local `dbgen`s.

Transport extraction has since moved administration HTTP/OIDC and browser assets
into `internal/admintransport`. The member gRPC handlers and clients currently
live in `internal/receiptstore`, not the proposed `internal/membertransport`
package below. SQL inputs and configuration are centralized under `db/` and the
root `sqlc.yaml`, while generated types remain private to each owning module.
Use the [code map](../code-map.md) for current paths. The table and mechanisms
below remain design direction; retry/revision/job/service-account behavior is
not implied by the transport extraction.

## Proposed responsibilities and dependency direction

Keep `cmd/fry`, `cmd/fry-member`, `cmd/fry-migrate` and `cmd/fry-admin` as the four
process entry points. Commands parse configuration, construct explicit dependencies,
start processes and own shutdown. They contain no permission, receipt or SQL rules.

| Location at design time | Proposed responsibility and interface |
| --- | --- |
| `internal/admin` application files | Administration module: named operations such as create credential, edit routing, inspect storage and submit maintenance. Own current action/scope/content authorization, revisions, audit and retry outcomes. Accept typed caller identity references and request values; return application values/errors, never transport or generated database values. |
| `internal/admin/http.go`, `oidc.go`, embedded `web` | Move transport responsibilities into `internal/admintransport`; retain browser assets beside this adapter. Authenticate local/OIDC sessions and service tokens, enforce browser origin/CSRF, map Connect requests and errors, serve assets and original downloads. OIDC persistence remains behind administration operations. |
| `cmd/fry/transport.go`, member listener wiring | Move protocol handling into `internal/membertransport`: Connect-Go handlers and gRPC client adapter, separate internal listener, mTLS peer identity, release matching, limits and cancellation. Expose no member methods on the administration listener. |
| `receiptstore` | Durable mail module: accept attempts, publish mailbox/outbound references, read immutable originals, manage retention/packing/member ownership and recovery. Preserve its receipt/completion invariants. Its existing `Member` seam has local and remote adapters; reuse and narrow it where necessary. |
| `maildirectory`, `credentials` | Routing/mail-credential module and password primitives. Preserve distinct administrator, SMTP and IMAP authentication namespaces; recheck mail-account/sender authority at the existing operation seams. |
| `smtpserver`, `imapserver`, `outbound` | Protocol adapters and outbound worker; delegate durable mail behavior. Retain SMTP/IMAP interoperability and check current authority on checked operations, including established sessions. |
| `imapcollect`, `imapmigrate`, `imapsource` | Collection/migration orchestration and source adapter. Persist resumable progress through receipt operations; keep source-network concerns outside durable mail rules. |
| `hotstore`, `objectstore`, `messageindex` | Concrete byte storage, object adapter and index implementation; preserve existing integrity, bounded access and resource ownership interfaces. |
| `installation`, `dbschema`, `replicaconfig`, `telemetry` | Schema installation, compatibility/configuration validation and explicit process telemetry. Keep environment parsing at command entry points. |

Dependencies flow command → transport/worker → application operation → durable
mail/directory modules → storage/database implementation. Adapters may import
generated protobuf types; application operations do not. `receiptstore` and
`maildirectory` never import administrator transport or administrator identities.
Keep sqlc query types private to each owning module; generated files remain
compiled, reproducible and integration-tested. Do not create generic repositories,
one interface per table, or duplicate domain models just to wrap sqlc.

## Transaction and durable-operation seam

The existing `admin.directoryChange`, `maildirectory.Put*` transaction arguments,
and `receiptstore.EnsureInboxInTransaction` are intentional atomic composition.
Replacing them with independently committing operations would split permissions,
business changes and audit. Retain narrow, explicitly transaction-bound domain
functions where cross-module atomicity requires them. These implementation-facing
functions accept `pgx.Tx`, never commit it, never open another connection, validate
organization ownership and hide generated queries. Transport callers cannot use
them; document their required locks and keep their callers confined to application
or installation composition. Do not spread unrestricted transaction callbacks
through public application interfaces merely to avoid these few concrete seams.

Administration owns each quick mutation's transaction: take existing policy locks,
resolve current authority, claim/check the scoped request ID and request fingerprint,
recognize a completed authorized retry before revision checking, atomically compare
the resource revision and mutate, append audit, record the non-secret outcome,
then commit. Concurrent identical requests serialize on the same durable claim.
A failed commit remains uncertain to the client; retry uses the same request ID.
No success response precedes commit. Secret material is absent from fingerprints,
logs and stored outcomes; fingerprint sensitive inputs through a reviewed keyed
construction rather than retaining plaintext or guessable password hashes.

Expiry recognition cannot be an absent-row check: the operation module must own
the ID-age/rejection-marker mechanism proposed in [API mechanisms](api-mechanisms.md)
alongside result retention. An
expired retry returns an explicit application error; no adapter invents a new ID.
Request-ID scope and expiry mechanism remain contract decisions, not settled here.

Long work atomically records submission, audit and operation identity before
returning. A process-owned worker resumes durable checkpoints, checks execution
authority, and records safe cancellation/completion truthfully. No goroutine
inherits a disconnected request as its work lifetime. External member/object/DNS
effects cannot be made atomic by holding a PostgreSQL transaction; use existing
receipt claims and operation-specific recoverable steps. Durable tracking alone
does not promise exactly-once external effects or production HA.

## Transfer, context and frontend seams

Expose original downloads as authenticated binary HTTPS through the same content
operation used by other callers. That operation checks authority and creates audit
intent before opening bytes, returns immutable identity/size/validator and an owned
reader, and records actual completion/interruption separately. The adapter handles
HTTP range syntax; the module validates authorized immutable range access and
bounded reads. Every resumed request reauthorizes. Do not hold a database
transaction while a slow client streams or report authorization as full delivery.
Internal gRPC uses bounded chunks with aggregate limits, integrity and explicit
completion; successful transport termination cannot manufacture durable acceptance.

Pass contexts on operations; inject clocks, DNS, byte-transfer and telemetry
dependencies where behavior actually varies. Keep telemetry providers process-owned
and constructor-supplied; context carries cancellation and trace propagation, not
a hidden dependency container or global runtime. Preserve disabled telemetry and
privacy guarantees. Cleanup has explicit ownership and bounded shutdown contexts.

Keep the existing embedded browser; introduce generated Connect-Web clients through
a small pinned build step and focused request/form modules. Keep ordinary DOM
rendering. Browser and CLI retain request IDs after uncertain outcomes, surface
revision conflicts, poll durable operations and never silently retry expired IDs.
Generated-client adoption does not justify adding React or another SPA framework.

## Verification and detailed contracts

Pure validation, permission evaluation, error mapping and stream framing use unit
tests at their existing module interfaces. Real PostgreSQL tests exercise current
authority, racing duplicates, stale edits, audit/outcome rollback, secret replay
omission and uncertain commits through application operations. Test transport
adapters over real HTTP/gRPC with cancellation, malformed chunks, mTLS denial and
resumed-download revocation; browser tests exercise actual forms and retry states.
Use real local/remote member adapters and object fixtures where they already vary;
add a fake only for a named failure case that cannot be induced reliably otherwise.
Keep the accepted package coverage ratchet and native architecture cadence.

Detailed retry/expiry, recovery, cancellation and download/cursor mechanisms are
specified in [API mechanisms](api-mechanisms.md), with numerical limits provisional.
The compatibility support window
is accepted: at least six months after both replacement stability and announcement.
Package moves, explicit constructors, private generated types and keeping one
PostgreSQL transaction are accepted engineering direction. Per-method recovery and
cancellation contracts must be detailed before implementing each method.
