# Explicit hot-file rotation and sealed reads

Status: implemented local prototype with [validation evidence](../validation/hot-file-rotation.md), following [bounded admission](bounded-hot-admission.md) and [capacity lanes](hot-capacity-lanes.md). This adds a legitimate producer of sealed files together with the read support needed to keep their accepted originals accessible. The later [automatic-rotation follow-up](automatic-hot-rotation.md) adds opt-in conditional triggering; concurrent reclamation remains separate.

## Keep the existing identities

A member remains the logical storage participant, its owner epoch fences a process, and each FileID names immutable physical bytes. Rotation changes the member’s current-file pointer. Existing targets, locators, lane assignments and receipt identities continue to name their original file. Sealing never redirects historical authority by interpreting an old FileID as the new current file.

The interface exposes an explicit `Store.RotateHot(ctx, memberID)` operation with corresponding authenticated remote-member operation/status handling. Bind maintenance operation identity to its kind, member, owner epoch and expected current FileID. A retry of rotation must not become a rewrite, or vice versa. Normal receipt preparation continues to reserve only the current file; the later conditional scheduler operates separately from admission.

## Publish a closed source and a durable successor

Retain the member-wide maintenance gate and committed admission exclusion. Establish a closed census of every source-file target and physical record. Preparing attempts block rotation; aborted targets require confirmed cancellation. Lost replies can leave physical records without recorded locators, so the actual inventory must be reconciled with registered obligations. No timeout alone proves closure.

Reserve the successor’s full ceiling before creating its directory. Initialize its durable header, register its exact FileID and empty capacity lanes, then revalidate the census, owner, budget and maintenance identity. Atomically make the source sealed with exact physical length/count, make the successor current, and switch the member pointer. Close the source writer before releasing maintenance exclusion.

Preserve source target reservations and lane counters conservatively, including closed aborted debt whose physical extent differs from its worst-case reservation. The sealed physical charge is exact file length; it is a different quantity from reserved admission bytes. Do not invent an equality between the two.

Rotation and rewrite jobs have distinct recovery semantics. A registered unpublished rotation successor may be abandoned only under the existing exact-file proof. Unregistered data remains fail-closed. Recovery after published rotation completes metadata; it must never unlink the sealed source. Ambiguous publication freezes admission and preserves both files. Fresh-schema initialization remains the only supported schema path.

## Read sealed originals without retaining their indexes

Resolve the current target and catalog state under the existing receipt/member guards and pins. A current-file target uses the existing byte store. A sealed target opens a read-only descriptor with verified FileID, regular-file identity and frozen length, then performs the existing full/partial integrity checks. The caller’s stale descriptor or locator never selects arbitrary bytes.

Hold at most one descriptor per sealed read, behind a small context-aware per-member permit bound. Do not retain per-file indexes or cache open descriptors. Keep the member gate for the actual descriptor lifetime, and close it before recording member-reader completion. The coordinator holds no member gate: each member adapter takes its permit and then only its own gate, so maintenance on an unused replica does not delay a read served by another member. Preserve the existing rule against falling back after partial output. A closure error whose resource state is uncertain must preserve pin debt and freeze unsafe member operations rather than manufacture proof of closure.

Startup validates sealed headers, catalog ownership and physical lengths before mutating the current byte store. It does not scan all sealed payloads. Corruption in untouched sealed bytes is therefore detected later than with a full startup scan; reads retain their normal frame/checksum verification. This is a deliberate bounded-index tradeoff.

## Preserve rewrite headroom

Let `C` be the current file ceiling, `H` staging headroom, `S` existing sealed physical bytes, and `s` the source’s frozen physical length. Before rotation, require the proposed state plus a spare rewrite file to fit:

`C + S + s + C + H <= member capacity`

The successor’s durable pre-creation charge and all other active/retiring obligations must also remain covered during the transition. This is a dynamic admission check, not a silent change to frozen profiles. A minimum `2*C+H` profile may safely refuse rotation; `3*C+H` permits the first worst-case full seal, but does not provide unlimited backlog capacity. Continuing to seal files eventually returns capacity until data is reclaimed.

The [sealed-file reclamation follow-up](sealed-file-reclamation.md) reclaims one sealed file under the same maintenance gate, preserving survivor lanes and physical copy order, publishing a sealed successor, and leaving the active current pointer unchanged. Its predecessor remains physically charged until durable unlink. Conditional automatic rotation has its own follow-up validation; relaxed maintenance concurrency remains outstanding.

## Required verification

Exercise local and authenticated remote operations through actual rotation, without fabricating sealed catalog authority in SQL. Verify unchanged historical locators, full/partial reads, new-current acceptance, restart reads, exact physical charges and refusal when spare capacity is missing. Include preparing/uncancelled abort blockers, lost-reply inventory, operation-kind replay conflicts, read-pin lifetimes, and failure cuts before initialization/registration, around publication, and after source-writer closure. A published rotation recovery test must explicitly prove the old source still exists and remains readable.
