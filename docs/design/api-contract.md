# API contract design

Implementation was authorized on 2026-10-18. This document records accepted
requirements, including work not yet implemented. Use the
[current implementation status](../implementation-status.md#current-engineering-status)
and [administration API gap list](../reference/admin-rpc.md#unavailable-foundations-and-compatibility)
to distinguish those requirements from current behavior.

Status: consolidated design accepted and implementation authorized, 2026-10-18. Numerical limits in the companion mechanism design require
validation. SMTP and IMAP remain the email protocols. The administration API
and internal member RPC are in this engineering phase; application-delivery
API/webhooks remain in their previously agreed later milestone.

## Accepted choices

- Protobuf defines the RPC contract. Connect-Go supplies handlers, Connect-Web
  supplies browser clients, and internal calls use gRPC over mutual TLS.
- Administration supports RPC-style HTTP/JSON as well as standard gRPC clients.
  REST resource URLs and HTTP-verb mapping are not required. No gRPC-Gateway is
  selected. See [ADR 0023](../adr/0023-use-connect-for-protobuf-rpc.md).
- Preserve the FRY server, member, migration tool and admin CLI process split.
  This does not require exposing internal member methods through the public
  administration listener.
- Preserve the previously agreed human/OIDC and scoped-service-account design.
  Current permission checks apply on each operation; content access is separately
  authorized and audited. A generated handler does not grant permissions.
- Internal peers require matching release versions initially. Linux amd64/arm64
  and independently deployable installations remain the target.

## Accepted client and documentation deliverables

The first release provides versioned protobuf definitions, generated Go and
TypeScript clients, and a readable RPC reference with examples. An OpenAPI
specification is not required initially. These artifacts follow the previously
agreed private repository/release policy; this scope decision does not authorize
publication. The HTTP original-download contract must also be documented because
it is not described by the RPC method definitions alone.

Exact packaging conventions remain to be specified. Generated
code does not replace documentation of permissions, errors, retry IDs, revisions,
operation lifetimes, secret handling or download behavior.

## Accepted payload transfer design

Use unary RPCs for administration/metadata. Download original email bytes through
an authenticated binary HTTP stream for the browser and CLI, with the same content
permissions/auditing as RPC. Internally, transfer data in bounded protobuf chunks
over gRPC, with aggregate limits, cancellation, integrity checks and explicit
completion semantics. A transport success must not replace the storage protocol's
durable acceptance decision.

The user accepted this split after comparing HTTP downloads with RPC streaming.
Downloads go through FRY, rather than exposing packed storage objects directly.
See [ADR 0024](../adr/0024-separate-downloads-from-internal-transfer.md).

This avoids representing a possible 200 MB original as one protobuf/JSON message.
Chunk size, transfer concurrency, time budgets and incomplete-stream handling
still need a concrete specification. Browser RPC streaming
support is narrower than native Go support; see the
[primary-source comparison](../research/api-transport-options.md).

### Accepted download resumption behavior

Support resumable original-email downloads in the initial release when the client
supports resumption. Every resumed request rechecks current content access and
verifies that it addresses the same immutable original. It must not append bytes
from a different original to a partial download. The client must restart if a
safe resume cannot be established, and only while a fresh download is authorized
and the original remains available. Resumption does not grant continuing access
after permission revocation or change the existing retention policy.

The detailed HTTP range/validator contract, partial-transfer auditing and bounded
read protection remain to be specified within these accepted constraints.

## Accepted administrative retry behavior

Administrative mutations support client-generated request IDs. Retrying the same
operation with the same ID and request contents recognizes the recorded operation
instead of applying the change again. Reusing an ID with different contents is
rejected. Every retry checks current permissions before disclosing the outcome;
knowledge of a request ID does not preserve revoked access.

Retain a completed administrative operation's retry-deduplication record and
non-secret result for seven days after completion by default, with a configurable
period. Unfinished operations remain tracked rather than expiring under this
completed-result policy. Audit-log retention is a separate policy.

After a request's retry record expires, FRY rejects reuse of the old request ID.
The client must inspect the current state and deliberately submit a new request
ID if further action is needed. Neither the UI nor the CLI may silently replace
an expired ID and repeat the mutation. An expired outcome is not evidence that
the original operation failed or never ran.

The request-ID design must preserve the ability to distinguish expired IDs from
fresh submissions after full result records are removed. Deleting a deduplication
row and treating every absent ID as new would violate this contract. The mechanism
and its behavior when retention settings change remain design work.

Generated credential secrets are revealed only in the original creation response.
If that response is lost, an authorized retry identifies the created credential
and its outcome without returning the secret. An authorized administrator must
revoke and replace the credential to obtain a new usable secret. A retry must not
silently generate another credential. Completed-operation records and audit logs
must not retain generated secrets for later replay, and no secret-recovery window
is provided. Retrying a successful creation with the secret omitted must be clearly
distinguishable from a creation failure.

The implementation design must account for simultaneous duplicate requests and
failure between applying a mutation and recording its outcome. Request-ID scope
and expiry recognition remain to be specified. This agreement does not promise
unlimited result retention or automatic replay of credential secrets.

## Accepted concurrent-edit behavior

Updates to existing administrative configuration carry the revision the client
read. FRY rejects a stale revision instead of silently overwriting newer changes.
The client reloads the current state before deciding whether to submit a revised
edit. Checking the revision and applying the update must form one atomic
operation; a separate preflight check cannot prevent lost updates.

Retry deduplication and revision checks serve different purposes. An authorized
retry of an already completed request recognizes its recorded outcome rather
than reapplying the mutation against the resource's now-newer revision. A changed
edit is a new request, using the current revision and a new request ID. Exact
revision representation and coverage for deletions or multi-resource operations
remain to be specified.

## Accepted long-running operation behavior

Administrative work that may take minutes returns a durable operation ID promptly.
The UI or CLI can reconnect to check progress and retrieve its outcome. Work
continues independently of the submitting client connection; disconnecting or
timing out while waiting is not an instruction to cancel the submitted operation.
Ordinary quick mutations continue to return their result directly.

The accepted request-ID deduplication policy applies to submission: retrying an
accepted submission identifies the same operation instead of scheduling duplicate
work. Access to operation status and results remains subject to current
authorization and organization scope, including separate content permissions.

Explicit cancellation is supported where safe. Queued work can be canceled before
execution; running work stops at a safe point and reports completed changes.
Requesting cancellation is distinct from confirmation that work has stopped.
Cancellation does not automatically roll back completed changes. Operations that
cannot safely stop must expose that limitation. A completion racing with a cancel
request must remain truthfully reported; a cancel request cannot relabel completed
work as never performed. Cancellation follows the operation's authorization scope.

Durable operation tracking alone does not establish exactly-once execution or
safe recovery of every side effect. Operation-specific checkpointing, worker
ownership/recovery, execution-time authorization and cancellation points still
need explicit contracts. Completed non-secret results use the configurable
seven-day retention policy above. This API decision does not establish
the separate production HA/recovery guarantees.

## Accepted list navigation behavior

API lists and administration screens use bounded pages with next/previous
navigation. They do not promise exact totals or arbitrary page-number jumps in
the initial release. The UI may show the number of records loaded, without
presenting that number as the complete matching total. This applies to list
navigation; it does not remove separately agreed operational metrics or quotas.

Page-size limits, cursor representation, stable ordering, filter binding and
behavior while records change remain part of the concrete contract design.
The pagination choice alone does not promise a consistent snapshot across pages.

## Accepted administration API compatibility

After the first versioned release, preserve compatibility within each supported
administration API major version. Breaking changes require a new API major
version. This covers the behavior existing automation relies on, including
protobuf and HTTP/JSON consumers; wire-compatible schema changes alone do not
prove behavioral compatibility. Prototype breaking changes remain allowed before
that release.

Before an administration API major can be removed, clients receive at least six
months of overlap. The interval begins only when both the replacement is stable
and deprecation has been announced; the later of those events starts the clock.
The old major remains supported during that overlap. Merely introducing a new
major does not authorize immediate removal of the old one.

This policy does not change the initially required matching release versions for
internal FRY server/member processes. API-major naming, announcement mechanics and
client-package release conventions still need a concrete policy.

## Detailed mechanism baseline and remaining specification work

The [API mechanism design](api-mechanisms.md) supplies the accepted consolidated
direction for download ranges/validators, request-ID scope/expiry, revisions,
durable work, errors, cursors and deprecation announcements. Its numerical limits
remain provisional until validated during implementation. Remaining specification
work includes:

- Method-specific permissions, effect recovery and safe cancellation points.
- Validation of numerical limits and their coupled safety constraints, including
  retry admission, result retention and expiry recognition.
- Client artifact packaging and compatibility conventions; protobuf definitions,
  generated Go/TypeScript clients and an RPC reference are accepted, without an
  initial OpenAPI requirement.
- Protobuf validation and generation workflow; tested stable versions that agree
  on the Connect major version and its validation/telemetry integrations.

Earlier references to mechanisms still needing specification are resolved at the
design level by the linked mechanism document. Method-by-method permission, effect-recovery
and cancellation contracts must accompany the protobuf definitions before each
method is implemented; a generic job mechanism alone does not settle them.

Implementation proposals should centralize business authorization in application
operations. Transport adapters authenticate callers and map requests/errors;
neither bypasses organization scope or content-access rules. Internal services
need separate exposure/authentication from administration. These are design
constraints, not claims about a completed Connect implementation.
