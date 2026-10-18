# API mechanism design

Implementation authorization update, 2026-10-18: the user explicitly instructed
“start the work.” The earlier design-only pause is superseded. Execute the
accepted sequence, beginning with code quality and meaningful test baselines.
Delegated work uses medium or high effort and standard execution only; no low,
fast/priority, max or ultra modes.

Status: accepted as part of the consolidated design, 2026-10-18;
implementation is authorized. See [API contract](api-contract.md).
Numerical defaults below remain unbenchmarked proposals for validation during
implementation; acceptance does not establish their performance or suitability.
Accepted: Connect-Go administration HTTP/JSON and gRPC, internal gRPC/mTLS,
authenticated resumable HTTPS originals, bounded internal chunks, retry IDs,
seven-day configurable completed results, revisions, durable jobs, bounded list
navigation, administration-major compatibility and exact internal release matching.

## Request identity and bounded expiry recognition

- Require client-generated UUIDv7 IDs on administrative mutations. UUIDv7 embeds
  a millisecond Unix timestamp; its use here is FRY policy, not a UUID guarantee
  of trustworthy client time. See [RFC 9562 §5.7](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.7).
- Deduplication key: installation, stable authenticated principal ID, organization
  (or explicit global scope), fully qualified method including API major, request
  ID. Service-account credential rotation preserves principal identity. Another
  principal or method is a distinct operation; IDs do not deduplicate across them.
- Authenticate and authorize before revealing any outcome. Look up the key first:
  active records remain valid regardless of ID age; completed, unexpired records
  replay their non-secret outcome; expired records return `REQUEST_ID_EXPIRED`.
  Matching IDs with different request fingerprints return `REQUEST_ID_CONFLICT`.
- For absent keys, use database wall time after taking the admission-policy guard.
  Proposed first-use window: preceding 24 hours, at most five minutes in the
  future. Reject malformed IDs, future IDs, and IDs below the admission cutoff.
  Return database time and permitted window on clock errors, without auto-retrying
  with a new ID. ID generation happens when submitting, not when opening an editor.
- Persist a monotonically increasing admission cutoff: maximum of its stored value
  and database time minus the first-use window. First admission, policy changes
  and garbage collection coordinate through that guard. No change may lower this
  cutoff, including a clock rollback or enlargement of the admission window.
  Clock regressions may temporarily refuse fresh submissions; they cannot reopen
  expired IDs. A forward clock jump may shorten admission; investigate/correct time
  without lowering the cutoff. Database recovery must preserve this authority;
  loss of committed metadata is outside the claimed retry guarantee.
- Set each completed row's immutable expiry to completion time plus the configured
  retention, default seven days. Proposed minimum: 25 hours, exceeding the
  24-hour admission window plus five-minute future allowance. Configuration changes
  apply to later completions; increasing retention does not resurrect old results.
  Reject settings that violate this relation. Future-skew changes obey the same
  relation and never bypass the persisted cutoff.
- Delete an expired row only when its UUID timestamp is strictly below the cutoff.
  Any rare expired row not yet eligible stays as a bounded temporary rejection
  marker. Thus a deleted ID is permanently inadmissible without permanent per-ID
  tombstones. Cleanup must retain active operations and unresolved effect tracking.
  UUID timestamp forgery cannot bypass the age test for the *same* ID; changing
  timestamp bits creates a new ID, as does deliberately generating another UUID.
- Canonicalize validated semantic input, including field presence, update masks,
  revisions and normalized defaults; sort maps, preserve ordered lists, reject
  unknown mutation fields. JSON and protobuf produce the same fingerprint. Use a
  versioned keyed hash; retain required key versions while records exist. This
  avoids storing plaintext input secrets or guessing low-entropy secrets from an
  unkeyed fingerprint. Never log canonical inputs, secret values or fingerprints.
- Serialize simultaneous first submissions with a unique key and transaction.
  For database-only work, mutation, revision check and completed outcome commit
  atomically. An ambiguous commit is resolved using the same ID. Persist terminal
  business failures; authentication, malformed input and pre-admission throttling
  do not consume an ID. Retrying a stored failure returns that failure.
- Generated secrets exist only transiently for the winning initial response after
  commit. Store their authentication verifier and non-secret credential identity,
  never replay material. Retry returns success plus `secret_available=false` and
  replacement guidance. A crash before delivery still consumes the creation.

This provides scoped retry deduplication, not universal exactly-once execution.
Clients must inspect state before intentionally replacing expired request IDs.

## Revisions and durable work

- Use opaque revision strings. All configuration updates and deletions require
  the observed revision; creates use explicit absence/uniqueness conditions.
  Multi-resource changes supply every affected revision and check them atomically.
  A successful recorded retry precedes revision checking. Recreated resources get
  fresh identity/revision; no revision reset that permits an old edit to succeed.
- Submission atomically records request identity, operation ID and queued work.
  States: queued, running, succeeded, failed, canceled; cancellation-requested is
  a separate flag. Expose progress units, completed effects, timestamps and safe
  failure details; progress need not supply a percentage or predictable total.
- Workers use the storage design's database lease/epoch pattern where applicable:
  60-second claims, renewal before expiry, takeover increments epoch, and locked
  publication checks epoch and database time. Authorization is checked when work
  starts and at each separately authorized step. An already authorized step may
  finish; revocation prevents subsequent steps and disclosure of results.
- Every external effect needs a registered stable effect ID, durable checkpoint,
  target-side idempotency/fencing or an authoritative reconciliation query.
  Losing ownership prevents publication; a database lease alone cannot stop an
  in-flight remote effect. Uncertain effects remain tracked and block blind replay.
  A method without a safe recovery procedure must not advertise resumable execution.
- Cancellation before dispatch prevents work; afterward it stops at a documented
  safe checkpoint. Report effects already committed. Completion wins its actual
  race with cancellation. Poll defaults to two seconds with backoff to 30 seconds;
  client disconnect does not cancel. Completion starts the result-retention clock.
  These rules do not establish the separate production HA/recovery guarantees.

## HTTPS original downloads

- Propose `GET`/`HEAD /api/v1/originals/{receipt_id}` through FRY, using the existing
  authenticated browser session or CLI bearer authorization, never URL credentials.
  Each request checks organization/content permission and current availability
  before revealing validators, length or ranges. No redirect to a pack object.
- Strong immutable ETag identifies receipt identity plus exact-original digest and
  length; storage relocation preserves it. Serve `message/rfc822`, attachment
  disposition with a sanitized filename, `Cache-Control: private, no-store`,
  `Accept-Ranges: bytes`, and no content transformation/compression. Full responses
  carry original Content-Length; HEAD returns the same representation metadata.
- Support one byte range, including suffix/open-ended forms. Valid ranges return
  206 with Content-Range and selected Content-Length; unsatisfiable ranges return
  416 and `Content-Range: bytes */length` after authorization. Reject malformed or
  multiple ranges with a documented 400 error. An unsupported range unit is ignored.
- CLI resume sends Range plus strong `If-Match`; mismatch returns 412. Standard
  `If-Range` is supported: a mismatch returns a fresh 200, which a client must never
  append. Append only a 206 whose ETag, start offset and total length match the
  saved original; otherwise restart while authorized. HTTP semantics follow
  [RFC 9110 §§13–15](https://www.rfc-editor.org/rfc/rfc9110.html#section-13).
- Acquire bounded renewable storage read pins; retain the existing proposed
  five-minute maximum read lifetime. Pins do not extend data retention. Stop when
  renewal fails; resumption requires a new authorized, available read. Verify
  selected storage frames before sending; a range does not validate unread bytes.
  Truncation/corruption aborts the stream, never fabricates a successful completion.
- Audit authorized starts and final complete/interrupted/failed outcomes with
  principal, receipt, requested range and bytes written, without content or tokens.
  Bytes written are not proof of client receipt. Translate original-relative ranges
  inside receiptstore; bound aligned reads and never expose neighboring pack bytes.

## Internal transfer and transport defaults

- Propose 256 KiB data chunks, 1 MiB internal message ceiling, four concurrent
  transfers per member peer, 16 per process and 16 MiB aggregate queued payload.
  Admission/backpressure also reserve staging capacity; limits are tunable and
  unbenchmarked. Unary administration requests/responses are capped at 1 MiB.
- Transfer header binds attempt, operation, member incarnation, epoch and expected
  length; ordered chunks carry offsets. Enforce bounded total length against the
  configured original limit, validate final length/SHA-256, and reject gaps/repeats.
  No inline whole-original protobuf/JSON response, including a 200 MB original.
- Propose 30-second no-progress timeout and five-minute per-stream ceiling; canceled
  streams leave tracked incomplete work for cleanup/reconciliation. Durable ack
  requires storage fsync/identity checks; RPC OK alone cannot establish receipt
  acceptance. Lost ack queries authoritative operation status before retransmission.
  [Storage protocol](storage-protocol.md) remains the publication authority.

## Errors, pagination and compatibility

- Error details: stable reason, safe message, field violations, request/operation
  ID when known, retry delay when appropriate and authorized current revision.
  Use invalid_argument for input, aborted for revision conflicts, already_exists
  for ID conflicts, failed_precondition for expired IDs, resource_exhausted for
  limits and unavailable for transient service failure. Uncertain outcomes direct
  same-ID lookup/retry; never silently create another ID. Conceal cross-scope data.
- Pages default to 50, maximum 200. Use immutable creation order plus unique ID,
  with authenticated opaque cursors bound to scope, filters, sort and direction;
  24-hour cursor expiry, next/previous only, no exact total. Reauthorize each page.
  No snapshot promise: membership changes can omit newly matching earlier items;
  previous navigation need not reproduce the exact earlier page. Invalid/expired
  cursors require an explicit restart. Keep cursor signing keys through expiry.
- Propose `fry.admin.v1` and matching versioned download paths. Reserve deleted
  protobuf fields; preserve documented JSON and behavioral semantics within major.
  Internal peers reject mismatched release identity before admitting work. The
  accepted administration-major policy requires at least six months of supported
  overlap starting only after both a stable replacement and a deprecation
  announcement. Proposed announcement channels are the versioned RPC reference,
  release notes and an administration UI notice naming the earliest removal date.
  Client-package conventions remain detailed design work.
