---
status: accepted
---

# Reclaim sealed files independently

Reclaim one explicitly selected sealed file under the existing member maintenance gate, validating its complete bounded inventory and preserving the current writer's identity. Publish a sealed successor for surviving records, or a durable delete-only outcome when none remain, so recovery never infers deletion authority from a missing successor. Keep physical charges until proven descriptor closure and durable unlink; this bounds census memory to one file while accepting a member-wide pause during reclamation.

See the [design](../design/sealed-file-reclamation.md) for capacity, selection and recovery constraints, and the [local validation](../validation/sealed-file-reclamation.md) for race, recovery and fresh-restore evidence.
