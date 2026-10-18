---
status: accepted
---

# Partition hot-file capacity

Use a small immutable set of capacity lanes within each bounded file, with record and byte ceilings summing to the file limits and each target retaining its exact lane reservation through retries and rewrites. Shared accounting serialized preparation and refunds; [independent-progress tests and local measurements](../validation/hot-capacity-lanes.md) justify partitioning for the bounded-member prototype, while preserving the aggregate capacity guarantee. Accept conservative fragmentation: spare aggregate bytes or slots do not guarantee that one lane can admit another maximum-sized reservation.

The [implementation design](../design/hot-capacity-lanes.md) freezes layout version/count and permits retry only after confirmed whole-transaction rollback. Physical file charges, uncertain-write debt and durable-unlink release remain unchanged. Borrowing between lanes, durable allocation grants and automatic segment rotation remain separate decisions; these local measurements do not establish production throughput.
