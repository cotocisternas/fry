# Sealed-file reclamation

Status: implemented local prototype with [validation evidence](../validation/sealed-file-reclamation.md), following [explicit rotation](hot-file-rotation.md). This does not establish a production erasure deadline or availability guarantee.

## Select one file and preserve the writer

`Store.ReclaimSealed(ctx, member)` selects one sealed file under the existing member-wide gate and committed admission exclusion. The corresponding authenticated remote operation is bound to kind, member, owner epoch and expected current FileID. Its selected source is durable: retrying an operation cannot silently select another file. `ReclaimHot` retains its current-file behavior.

A process-local file cursor advances before inspecting the candidate, including attempts that do no work or fail. With a fixed finite set and continued successful scheduling, a blocked or unchanged earlier file cannot monopolize selection. Cursor state does not promise fairness across restarts, multiple coordinators or indefinitely changing file sets.

The running coordinator's existing `reclaim_hot` schedule alternates complete member sweeps over current and sealed files, with one attempt per configured interval. Each class consequently receives a turn every two sweeps. Ordinary lifecycle work and reclamation share the cycle deadline. Automatic rotation remains separate.

## Establish a complete bounded census

Open the selected source through the read-only exact-FileID/exact-length image interface. Inventory validates the entire file, including framing, checksums, full-message digests, duplicate record identities and the catalog's frozen count and length. It returns physical-order locators only after complete success. Cancellation or corruption yields no usable prefix. Limits apply before record/payload work and allocation; no global index, retained payload cache, staging cleanup or writable recovery is involved.

Scan outside a database transaction. Under the existing attempt-then-receipt lock order, reconcile every physical record and registered target. Unknown records, mismatched locators, preparing attempts and unconfirmed aborted writes prevent reclamation. Confirmed absent writes and lost replies must be distinguished from unknown physical obligations. Read pins and surviving receipt requirements preserve records conservatively.

Keep at most one capped source census and copy survivors in physical-offset order, preserving their immutable capacity-lane assignments. Recheck authority and the census before publication. The member's active current FileID, directory and writer handle do not change.

Exclusive maintenance opens one image without acquiring a normal-reader permit. Actual readers have already drained through the member gate; queued readers may hold permits while waiting for that gate, so maintenance waiting for those permits would invert the lock order.

## Publish either survivors or deletion

For surviving records, reserve the successor file ceiling durably before creating its directory. All current, sealed, retiring, building and preregistered obligations plus staging headroom must fit the member budget. Register the exact new file and its empty lanes, copy the survivors, and atomically publish new locators and manifest versions, exact lane occupancy, a sealed successor and a retiring predecessor.

For zero survivors, persist an explicit delete-only outcome and retire the source without creating an empty sealed file or reserving a replacement ceiling. Absence of a successor is never itself evidence that deletion was authorized.

Prove closure of every inventory/copy descriptor before destructive cleanup. A lost closure proof or uncertain commit freezes the member and retains files and accounting debt. The predecessor remains charged until exact-source verification, unlink and directory synchronization complete. Only then clear its charge and lanes and mark remaining predecessor targets removed.

## Recover by operation kind and phase

A building job can discard only a proven unpublished successor. A published sealed-reclaim job completes predecessor cleanup without switching the current writer. Rotation recovery continues to preserve its sealed source. Catalog provenance distinguishes a rotation's source from a reclaim's successor, and historical successors may later be reclaimed themselves.

Fresh-schema initialization remains the supported schema path. Unclean-owner takeover, automatic repair and relaxed maintenance concurrency remain separate work.

## Verification gate

Use actual rotation to produce sources. Cover mixed survivors, zero survivors, unchanged files, multiple-file selection, active writer identity and subsequent acceptance, bounded inventories, corrupt/truncated files, independent-replica reads, local/remote reader lifetimes and lost closure proof. Exercise capacity exhaustion, exact lane accounting, lost append replies, operation replay/kind/owner binding, building/published/unlink recovery cuts and clean reopen. Run the complete PostgreSQL-backed race suite and a fresh offline restore before adopting the decision.
