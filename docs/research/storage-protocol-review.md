# Review of the storage protocol draft

Reviewed 2026-10-17 against [storage-protocol.md](../design/storage-protocol.md) and [verified platform foundations](storage-protocol-foundations.md). Design review only; no implementation or failure tests ran. The S3 permanent-marker approach addresses the identified late conditional-PUT race under its stated restrictions. The following details should be fixed before treating the protocol as implementation-ready.

## Must make explicit

1. **Register every hot-write destination before dispatch and fence late local work.** Receipt steps register the attempt, but do not explicitly persist target member/incarnation and staging-operation identities before sending bytes. If the coordinator dies before receiving locators, GC must still discover every possible copy. Attempt abortion must revoke publication and transition each member's operation under its local operation/writer lock; late staging completion, append, or retry cannot create untracked bytes after cleanup is marked complete. Keep cleanup obligations/tombstones until those operations are terminal or the member remains recorded as inaccessible. Sealing one append file alone does not prevent a paused operation from choosing a different file. This applies to repair and compaction staging too. [Receipt steps and orphan handling](../design/storage-protocol.md:40)

2. **Specify hot-file reclamation separately from S3-pack reclamation.** Multiple receipts share append files, so an expired record cannot be reclaimed by unlinking its individual locator. Introduce an explicit sealed-file lifecycle/inventory: retire a file only after every record has a safe successor or terminal cleanup status, including unaccepted attempts and active handles. Mixed-live hot files need survivor copying or another specified safe reclamation method; bounded sealing must prevent an indefinitely open file from delaying the oldest cleanup deadline. The overview recognizes this, but the protocol's detailed reclamation section primarily describes S3 packs. [Reclamation](../design/storage-protocol.md:68)

3. **Name the common guards for each conflicting operation.** Pack publication should explicitly lock the pack-attempt guard shared with abandonment/retirement, then receipt/location guards in a documented total order. Read-pin acquisition must atomically validate receipt availability and the chosen location under guards shared with reference expiry and retirement. A check before locking only the location leaves a stale-lifecycle race. Specify pin renewal's refusal after retirement and prevent expiry from being renewed after GC has acted. The general locking rule is sound; these exact transactions remain underspecified. [Guard rule](../design/storage-protocol.md:36), [publication](../design/storage-protocol.md:54), [reads](../design/storage-protocol.md:64)

4. **Define restart and identity checks for byte records.** State the scanner's treatment of incomplete tails versus corruption inside a committed prefix; never truncate later acknowledged records. Validate receipt identity, encoded bounds, frame sequence, and locator generation before exposing bytes, not only payload CRCs. For full-message reads, specify whole-message SHA-256 verification and failure reporting; partial reads can validate only the addressed frames. [Format/read contract](../design/storage-protocol.md:62)

## Evidence gates, not new design blockers

Permanent S3 markers require enforced create-only producer permissions, no marker deletion, and a never-versioned bucket; test simultaneous PUT/retirement and lost responses against AWS. Their permanent catalog growth needs accounting.

Normal versus degraded acceptance already records protection state; record actual member/AZ placement as well and test profile changes racing commit. Metadata availability is mandatory, and reduced protection cannot manufacture unavailable acknowledged bytes.

The draft correctly states that inaccessible disks/S3 and lingering handles can violate the 24-hour target. Preserve visible overdue obligations and nonserving return reconciliation. This is an explicit availability limitation, not demonstrated deadline compliance. No fatal issue was found with the overall PostgreSQL-authority approach; its safety depends on closing these concrete state transitions.

## Follow-up resolution review — 2026-10-17

The revised protocol addresses all four findings at design level:

- `ingest_target` and hot-file inventory precede dispatch; member cancellation shares the operation lock, queued retries recheck authoritative state, and `cancel_confirmed` is distinct from `bytes_removed`.
- Hot-file reclamation now seals/inventories files and preserves needed records through durable same-AZ survivor copies before retiring the original. Such copying is explicitly not extra AZ redundancy.
- A total guard order and shared pack, receipt, container, and pin guards now cover publication, retirement, and pin acquisition/renewal.
- Restart handling distinguishes incomplete unacknowledged tails from interior corruption; reads validate identities/bounds/frame order, with full-read SHA-256 and a narrower partial-read guarantee.

No critical remaining contradiction was identified in this bounded reread. These are specified safeguards, not validated implementations.

Also inspected [the executable state model](../validation/storage_state_model.py). Its main explorer represents one receipt, two hot-copy AZs, one pack key, two worker epochs, one reader pin, and at most one AZ loss. It assumes atomic durable DB operations, truthful byte confirmations, and conditional-object semantics; it does not exercise SQL locking, real disk writes, AWS, timing, or recovery throughput. The six-order mixed-pack helper is a small membership illustration, not exhaustive compaction verification. State counts and detected mutants must be reported within those limits; they do not establish five-minute recovery or 24-hour cleanup. This review inspected the code and recorded report, without independently rerunning the model.
