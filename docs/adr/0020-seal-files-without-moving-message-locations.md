---
status: accepted
---

# Seal files without moving message locations

Add explicit hot-file rotation together with authoritative sealed-file reads: switch only the current writer pointer while existing targets keep their exact FileIDs and locators. Retain the member-wide maintenance gate, distinguish rotation from rewrite recovery, and require spare rewrite capacity before sealing so a successful rotation cannot consume the space needed to reclaim retained bytes later. Read sealed files through bounded read-only descriptors instead of retaining all sealed indexes, accepting lazy detection of untouched payload corruption.

The [implemented local design](../design/hot-file-rotation.md) preserves uncertain-write debt and physical charges. The [local validation](../validation/hot-file-rotation.md) covers failure cuts, bounded reads, owner binding and fresh restore across sealed/current files. Automatic rotation, sealed-file reclamation and concurrent maintenance remain later slices.
