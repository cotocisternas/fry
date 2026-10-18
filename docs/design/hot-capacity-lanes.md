# Independent hot-file capacity lanes

Status: implemented and adopted for the local bounded-member prototype, with [validation and measurements](../validation/hot-capacity-lanes.md). Production scale and infrastructure guarantees remain unvalidated. This follows the [bounded-admission measurements](../validation/bounded-hot-admission.md), which identify shared file-accounting updates as a throughput constraint. It changes internal accounting for the existing capped single-file interface; it does not add rotation or change accepted-email retention.

## Preserve the aggregate limit

Divide a bounded file's record ceiling `R` and its usable encoded-byte ceiling `C − header` among a small immutable set of lanes. Lane ceilings sum exactly to those totals; charge the file header once. Every fresh target reserves one slot and a maximum encoded record in one lane before bytes are dispatched. Settlement returns unused bytes to that same lane. An accepted retry, lost reply or aborted write never silently acquires a new lane.

The lane count is derived from the frozen member profile:

`L = min(16, concurrency, R, max(1, floor((C − header) / (8 * maximum_encoded_record))))`

Persist layout version 1 and the derived lane count in the member profile. Legacy profiles use zero lanes. Each `fry_member_file_lane` row stores one partition’s immutable ceilings and current reservations; each bounded target retains `capacity_lane` and `capacity_reserved_bytes`, linked to that exact file and lane by a foreign key. Use checked arithmetic and distribute integer remainders without increasing aggregate ceilings. Small files and large maximum messages can reduce the layout to one lane; there is no parallelism claim for that case. This prototype rule limits per-lane worst-case byte tails when multiple lanes fit. It does **not** bound combined record/byte fragmentation: small messages can exhaust one lane's slots while large messages exhaust another lane's bytes.

A fresh snapshot with no lane that can satisfy both requirements may conservatively defer admission even when aggregate unused space remains. That means no eligible accounting partition, not necessarily a full physical disk. Capacity borrowing or independent record/byte allocators would require another design and are not hidden in this experiment.

## Reserve both replicas atomically

Keep the existing stable member ordering and acquire both member guards before reserving capacity. Select one eligible lane per member with a materialized candidate query, using a deterministic hash-derived starting lane and rotated preference on each retry. Acquire it normally and repeat both capacity predicates in the update. A dependent target update stamps the returned lane; both updates must affect exactly one row. A busy row is not proof of exhaustion; do not interpret `SKIP LOCKED` returning no rows as a full file.

If the chosen lane loses its capacity race while waiting, roll back the whole preparation transaction and confirm rollback before an internal bounded retry. Hold at most one acquired lane per member per transaction. Never probe another lane while retaining a failed lane lock in a conflicting order. At most eight preparation transactions are attempted, honoring cancellation. Exhausting this retry bound reports busy; an attempted or uncertain commit follows the existing uncertainty protocol and is never retried as an ordinary capacity race. No source bytes are read before a successful reservation commit.

## Keep lifecycle accounting exact

Current occupancy comes from the bounded lane set in one database snapshot. A stale aggregate counter on the catalog must not remain authoritative or be updated on every message. Startup checks the frozen layout, exact ceiling totals, valid occupancy and physical file size against the summed reservations before byte-store mutation.

The physical-capacity ledger stays unchanged: current/building files consume their full ceiling, a rewrite job reserves its successor before directory creation, and retiring files consume exact frozen bytes until durable unlink. Lanes partition admission inside that charge; they do not replace it or create extra capacity.

Keep each surviving target's lane during rewrite, with identical successor ceilings. The complete per-lane survivor census is a subset of the source lane's obligations, so it fits without rebalancing. Create successor lane rows atomically with catalog registration before any target can reference them. Publish the survivor counts with locator changes, retain physical append order for byte-store copies and the existing receipt order for authority locks. After durable unlink, zero the old lane counters but retain their rows for historical target foreign keys. Uncertain, unregistered and aborted-copy rules remain in force.

## Validated local gates

- Prove a genuine refund can complete through one lane while another lane's counter is locked; prove the same-lane dependency still blocks correctly.
- Preserve zero-delta progress, exact global record/byte caps, both-replica rollback, once-only settlement and immutable retry reservations.
- Exercise small layouts, integer remainder distribution, skew and explicit fragmentation without mistaking contention for exhaustion.
- Preserve closed-write census, survivor lane identity, byte integrity, predecessor charges and failure-cut recovery.
- Reject missing or contradictory layouts before filesystem mutation; exercise a full offline restore with populated lane accounting.
- Rerun the same exact-size and real-refund intake measurements, sequentially with frozen source, and retain unsuccessful results as well as successful ones.

Fresh-schema initialization remains the only supported schema path. Existing installations do not receive an implicit capacity conversion, layout rebalance or mode change.
