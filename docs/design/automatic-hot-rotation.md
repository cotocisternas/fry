# Conditional automatic hot-file rotation

Status: implemented local prototype with [validation evidence](../validation/automatic-hot-rotation.md), following [explicit rotation](hot-file-rotation.md) and [sealed reclamation](sealed-file-reclamation.md).

## Rotate only when the owner proves it is needed

`Store.RotateHotIfNeeded(ctx, member)` is a best-effort maintenance operation. It is separate from receipt preparation and explicit `RotateHot`; neither a failed database update nor an arbitrary capacity error triggers a rotation inside the admission transaction.

The owner first attempts the exclusive member gate without waiting. Busy readers or writers return `ErrBusy`. Under that gate, commit the normal maintenance admission exclusion, then examine the exact current file's bounded capacity lanes. Rotation is eligible only when no lane has both a free record slot and enough unreserved bytes for one maximum-size encoded original. This deliberately includes conservative lane fragmentation; aggregate unused space does not imply an admissible reservation.

Existing writers hold the member read gate through capacity settlement, so their completed refunds are visible before this check. New preparations are excluded by the committed maintenance gate. A bounded nonempty eligible file must still pass the complete closed-attempt census and existing spare-capacity checks before sealing. Preparing attempts, unconfirmed cancellation and uncertainty remain blockers. Empty files with confirmed aborted reservation debt are left for current-file reclamation; legacy profiles and files with available lanes are no-ops.

The physical transition uses the established rotation protocol and `rotate` job recovery. Maintenance operations use the distinct durable kind `auto-rotate`. A completed no-op is still complete: replay cannot evaluate a later full file and turn an earlier no-op into a new rotation.

## Keep remote discovery out of the blocking path

The authenticated `/v1/rotate-if-needed` route bypasses the generic gate-dependent identity check and attempts the exclusive gate first. It captures any returned current identity while holding the gate and writes its response after unlocking. The status route checks the recorded kind, member and owner epoch.

The remote adapter attempts its identity-refresh mutex without waiting and makes no separate `/identity` request before or after the operation. A stale expected FileID returns an authenticated current identity; the adapter validates the same member, failure domain and owner epoch, updates its cache under the existing mutex order, and returns `ErrStale`. A later scheduled turn uses a new operation and the updated identity. An existing completed operation is resolved before considering current-file drift. A changed owner epoch still requires the established reconnect path; automatic rotation does not acquire a new process owner.

## Schedule explicitly

The coordinator accepts `storage_lifecycle.rotate_hot`, default `false`. Enabling it requires `reclaim_hot: true`, so rotation cannot be configured without the existing cleanup scheduler. For example, a hot-only prototype can use:

```json
{
  "storage_lifecycle": {
    "interval_seconds": 1,
    "work_timeout_seconds": 10,
    "max_actions": 2,
    "max_receipts": 128,
    "reclaim_hot": true,
    "rotate_hot": true,
    "reclaim_interval_seconds": 5
  }
}
```

This adds a third complete member sweep after current-file and sealed-file reclamation. Each scheduled opportunity attempts one member and advances even on no-op, busy or stale results. The interval is shared by these three classes; it is not a per-member promise. When rotation is disabled, the existing two-class schedule remains.

Fair opportunities do not guarantee successful rotation. Continuous readers can starve the nonblocking gate, unresolved attempts can block the census, and capacity may remain insufficient. Existing manual reclamation paths retain their blocking gate acquisition, so a nominal worker deadline does not strictly bound all gate wait time. SMTP deferral and paused collection remain the response to unavailable safe capacity. This slice does not establish an intake-latency, outage-buffer or production-throughput guarantee.
