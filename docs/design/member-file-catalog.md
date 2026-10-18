# Explicit hot-file lifecycle

Implemented and covered by [catalog validation](../validation/member-file-catalog.md), including the full race suite, hot-only restore rehearsal and local acceptance/SMTP measurements. This slice strengthens file identity and deletion checks in the existing whole-member rewrite. It does not yet permit reception during that member's rewrite, rotate append segments, bound the retained index, or establish an availability guarantee.

The [retained-data measurements](../validation/retained-storage.md) show why later work must address both record-count memory and whole-member maintenance. The file catalog is a prerequisite for that work, not a measured performance improvement. [ADR 0017](../adr/0017-track-hot-file-lifecycles-explicitly.md) records the choice.

## Preserve the existing identities

`member` identifies a logical storage member. `owner_epoch` and its process instance fence process ownership. `incarnation` already means the immutable hotstore `FileID`, including in registered targets, read records and maintenance requests. Keep those meanings. Rewriting a file changes its physical identity without changing the logical member or current process epoch.

There is no new volume identity or transport identity in this slice. `fry_member.incarnation` and `directory` continue to point at the current file. Accepted retries still resolve through receipt authority, and remote reads retain their existing resolution of stale file hints under current receipt and read-pin authority.

## Catalog and publication

Track each file's member, file ID, member-relative directory, lifecycle state and applicable rewrite job. File identity and directory never change. A member has one current file; historical removed rows remain as evidence. A successor follows `building → current → retiring → removed`; an unpublished registered successor can instead follow `building → removed` when abandoned.

Register a fresh member and its current catalog row in one database transaction. Existing members require a matching catalog before opening their hotstore. Compare the current catalog, member pointer and actual file header through a read-only inspection: normal hotstore opening can create a missing file, sync it or remove staging files, so it cannot be the first check of an inconsistent mapping. Missing or contradictory metadata blocks opening; there is no implicit historical backfill.

For a rewrite, retain the existing member-wide gate, preparing-attempt prohibition, receipt guards and process ownership rules:

1. Persist the existing building rewrite job, reserving its exact successor directory.
2. Create and durably initialize the successor. Register its actual file ID and building catalog row under that job before copying survivors.
3. Copy and verify survivors through the current byte-store interface.
4. In the existing guarded publication transaction, update surviving target locators, the member's current-file pointer, both catalog states and the rewrite job together. Old current becomes retiring; new building becomes current. Keep existing lock ordering; catalog existence alone never authorizes writes.
5. Close the old file's actual handles and reconcile its deletion. Verify the exact retiring catalog row, job, directory and file identity before removing bytes. Make removal durable before recording removed catalog state and existing target/cleanup completion.

An uncertain publication preserves both files and freezes unsafe operations until authority is resolved, as in the existing protocol. An absent file after a known publication can represent a completed unlink whose metadata update was interrupted; resolution still requires the exact job and catalog history. A different file at that pathname is a conflict, not an invitation to delete it.

## Unregistered successor window

The durable rewrite job can precede the successor's creation and catalog registration. If a data file exists but registration never became authoritative, preserve it and fail closed in this first slice. Do not adopt its file ID or infer deletion permission from a missing catalog row or a `generation-` prefix. Only a proven job-reserved absent or empty directory may be cleaned without a registered file.

This conservative window can require later explicit reconciliation. It is a known availability limitation, not accepted-mail loss or proof of automatic crash recovery. Failed initial member registration likewise must not silently adopt unidentified existing bytes.

## Verification gates

- Fresh registration and clean reopen preserve exact member/file/catalog identity; missing schema, missing rows and conflicting header/path mappings fail before byte-store mutation.
- A real rewrite publishes exact new locators and file states while preserving the member and process epoch. Every surviving body remains readable directly from the rewritten replica.
- Prepared writes still block rewrite, accepted replay remains idempotent afterward, and pinned local/remote reads keep their existing closure guarantees.
- Faults around successor registration, copy, publication and unlink preserve unresolved bytes or complete only the exact authorized cleanup. Lost replies must not create a second interpretation of file ownership.
- Wrong directory, header or catalog/job associations never cause deletion. Reopening an unclosed process generation remains forbidden; a cloned database or released lock is not trusted fencing.

Schema initialization remains fresh-only. Restoring a matching complete database and member roots is a separate check; upgrading existing schemas requires an explicit conversion that is outside this slice.

## Later segmentation remains separate

A catalog with one writable file does not make startup memory bounded. Later segmentation needs a durable seal protocol covering every preregistered write, including lost locator replies and aborted-copy debt, plus bounded index or locator-driven read work. Accepted receipt locators alone cannot enumerate all bytes that may have been written.

Using database locators to read sealed files could avoid loading every record index, but it would defer detection of corruption in untouched bytes compared with today's eager full scan. An on-disk index introduces its own publication and recovery contract. Neither alternative is selected here; both require evidence beyond this lifecycle change.
