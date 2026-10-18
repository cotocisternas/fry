# Bounded hot-file admission

Status: implemented local prototype, with [correctness and performance evidence](../validation/bounded-hot-admission.md). The subsequent [capacity-lane implementation](hot-capacity-lanes.md) reduces shared-counter contention, with conservative partition fragmentation. This extends the [member-file catalog](member-file-catalog.md) with optional hard file limits and durable capacity reservations. This admission slice alone does not rotate files; the subsequent [explicit rotation slice](hot-file-rotation.md) adds that operation. A full file defers new attempts until existing reclamation removes enough dead records; a file full of live mail remains full. Do not enable small limits as a substitute for production segmentation.

## Configuration and identities

Set both `segment_max_records` and `segment_max_bytes` to positive integers to enable a bounded member. Omit both, or set both to zero, to preserve legacy behavior. Mixed, negative, overflowing or unusable settings are rejected. In local deployments these are top-level fields in the shared FRY storage configuration; for remote replicas set them on each `fry-member`, not the coordinator. The coordinator uses the persisted member profile when preparing writes.

Each bounded member freezes its maximum message size, concurrency, capacity and two file limits in PostgreSQL. Reopening with different bounded values, or changing between bounded and legacy modes, fails before byte-store mutation. Legacy numeric option changes remain compatible; stored legacy values are not an enforced sizing contract. Existing member, process-owner and immutable physical file identities keep their meanings.

Let `E` be the maximum complete encoded record size, `N` the maximum concurrent appends, and `C` the file byte ceiling. A file must fit its header and one maximum record. The member must fit `2*C + H`, where `H = 2*N*E` reserves staging/copy headroom. All arithmetic is checked. This is an encoded-byte budget; filesystem allocation overhead and process memory need additional operating headroom. No production record cap or file size is selected by this change.

## Prepare, append and settle

Receipt preparation reserves one record slot and `E` bytes for each exact target in the same transaction. Targets are visited in stable member order. If either target lacks capacity, neither reservation commits. Retrying an existing target reuses its reservation; an already accepted attempt returns its existing result.

File occupancy includes the file header, confirmed encoded records and unresolved worst-case reservations. Immutable per-file lanes partition the record/byte ceiling; each target retains its lane, and aggregate occupancy is the bounded lane sum plus the header. Publishing a durable locator settles its reservation to the actual encoded length exactly once. A missing reply, expired lease or aborted attempt does not release capacity: bytes can exist without a recorded locator. Existing cancellation and complete rewrite inventory establish when those obligations can be removed.

The byte store independently bounds actual file size and record count. Concurrent staging reserves possible growth before reading the source; an identical already stored key can still be retried at a full file limit, with content verification. Opening checks file length before scanning and enforces the record cap during scanning, before building an oversized retained index.

## Rewrite and deletion

Keep the existing member-wide maintenance gate and receipt/read/owner fencing. Reserve a successor's full `C` bytes durably on its rewrite job before creating its directory. Registering its actual file identity transfers that charge into the building catalog row. Publication records the survivor census, makes the successor current and freezes the old file's actual physical length as retiring occupancy.

Current and building files each consume their full byte ceiling; retiring files retain their frozen physical charge until exact authorized unlink is durable. Unregistered or ambiguous files remain subject to the existing fail-closed recovery rules. A failed publication cannot make both files disappear from capacity accounting. Reclamation still pauses the member and rewrites one current file; it does not permit concurrent ingestion into another segment.

Copy survivors in their original physical append order while retaining the existing receipt order for database guards and authority comparison. Each surviving prefix is then a subset of a prefix that previously passed worst-case admission. Copying in receipt-ID order could otherwise exhaust worst-case headroom midway through a rewrite even when the final surviving bytes fit.

## Verification and next steps

Required checks cover competing reservations, rollback when the second replica is full, replay and lost-copy replies, profile mismatch before filesystem mutation, bounded startup, and rewrite failure cuts before and after publication/unlink. Measure bounded-mode intake separately. The initial shared file counter serialized preparations and refunds; [partitioned accounting measurements](../validation/hot-capacity-lanes.md) show local improvement while keeping utilization and production-throughput limits explicit. Existing legacy-mode measurements do not establish bounded-mode cost.

The subsequent [explicit rotation slice](hot-file-rotation.md) adds a seal protocol covering all exact-file obligations and locator-based sealed reads. Reclaiming one sealed file under the existing maintenance gate comes before relaxing that gate for concurrent active-file writes. Fresh-schema initialization remains the only supported schema path; enabling limits on an existing deployment needs an explicit conversion design.
