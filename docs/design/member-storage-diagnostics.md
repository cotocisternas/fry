# Bounded member-storage diagnostics

Status: implemented prototype, with [focused validation](../validation/member-storage-diagnostics.md)
passed on 2026-10-17, including the full race suite and real-process rehearsal. This
read-only operator surface is not production validation, a new admission check,
a health guarantee, or a recovery procedure.

An organization quota measures retained originals. It cannot explain why a
shared storage member has exhausted its current file, is blocked by maintenance,
or has frozen after losing proof of closure. Global operators need those facts
without querying production tables manually or issuing a maintenance command.

## Scope and interface

The implementation provides one mandatory-member API request, a matching
administration CLI command, and a global-operator web view. It reuses the configured ingestion catalog, existing metadata authority and
authenticated member transport. Do not discover arbitrary roots, contact an
operator-supplied endpoint, change ownership, refresh remote authority, rotate a
file or run cleanup as a side effect of inspection.

The receipt-store operation reads one repeatable-read metadata snapshot. It
includes the configured profile, current file and at most 16 capacity lanes,
plus a keyset page of at most 64 file-catalog rows. The extra row detects whether
another page exists; it is not an unbounded count. Each response identifies its
database observation time. Successive pages may observe subsequent changes;
they are not a consistent backup inventory.

Release the database transaction before sampling the running owner. The local
sample attempts a read gate without waiting. A busy gate returns unknown
guarded fields; it must not infer that the member is healthy or unfrozen. The
remote path uses a separate authenticated diagnostic route with a bounded
deadline, and captures values before releasing the gate and writing the HTTP
response. Inspection must not hold a member gate while waiting for a database
connection, a filesystem operation or a remote client.

Runtime and metadata observations are deliberately separate. They have distinct
times and may show different current files during rotation. An unavailable RPC
does not prove that its owner has closed. A changed remote owner generation is
reported as a mismatch and cannot silently rebind the coordinator. No diagnostic
response authorizes admission, deletion, takeover or release of closure debt.

## Accounting semantics

Report frozen capacity settings, lane ceilings and reservations with their
existing meaning. A lane-fit observation describes the database snapshot alone;
it does not promise that the next message will be accepted. Owner state, ongoing
work and subsequent reservations can change the result.

File charges are conservative accounting obligations. A current or building
file may reserve its complete configured ceiling, while a sealed file records
its closed length. Staging headroom is a reservation, not a measurement of
currently occupied blocks. Legacy unbounded profiles must not display their
historical settings as enforced bounded-profile guarantees.

The first slice omits aggregate member totals, filesystem free space, S3 use,
filesystem overhead and organization attribution. Summing the displayed page
would not produce an installation total. These limitations must remain visible
in the response terminology and operational documentation.

## Authorization and validation

Only global operators may inspect shared member topology and state. Assigned
organization managers cannot access it, even if they manage multiple customer
organizations. Authenticate before collecting diagnostics, release the
authorization transaction before taking another pool connection or making an
RPC, and recheck authorization before returning the result. Do not expose paths,
private endpoints, credentials or message content.

Focused tests exercise real PostgreSQL metadata, indexed bounded pagination,
full current lanes, held maintenance gates, frozen closure debt, authenticated
remote identity mismatch and global-versus-organization
authorization. One-connection pool tests check for nested checkouts.
Revocation while diagnostics are in progress prevents the result from being
returned. Runtime tests distinguish an actual busy response from a test-side timeout;
the real-process rehearsal also checks unavailable owners. These checks do not establish production request latency or
availability guarantees.

## Web administration

The **Member storage** tab is available to global operators without selecting an
organization. Enter a configured member ID; the interface does not discover or
contact arbitrary members. Metadata, capacity lanes, owner proof and the later
runtime observation appear separately. Exact decimal strings preserve counters
above JavaScript's safe integer range. Non-sampled runtime fields remain unknown.

File history requests at most 64 rows. Next advances the returned keyset cursor;
Refresh starts again at the first page. Neither action runs maintenance. A failed
lookup clears the displayed snapshot and permits another lookup. Navigation and
session changes discard late diagnostic results; signing out clears the member
ID and prior results. This manual inspection view does not collect background
metrics, measure free space, or deliver alerts.
