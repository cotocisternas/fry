# Offline hot-only restore rehearsal

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

**Passed on 2026-10-17** with exit 0. The disposable local rehearsal restored
five saved originals and independently verified every one through each of two
hot replicas, restored one queued synthetic handoff, and accepted one new
original with UID continuity. Two damaged/incomplete archive variants were
rejected. The manifest contained 10 files and five directories; PostgreSQL was
`18.6 (Debian 18.6-1.pgdg13+2)`. Final success followed checked cleanup. Evidence:
`/tmp/fry-offline-restore-final.log` (temporary machine-local artifact).
No production resources or retained mail were used.

The rehearsal covers a cleanly stopped installation whose accepted originals
remain entirely in hot members. The recovery unit is the complete authoritative
PostgreSQL database **together with every configured member root**. A database
dump without bytes, a member subset, or independently copied live files is not
a valid backup set. This hot-only run covers no S3 objects or cold-pack restoration;
the separate [cold-pack rehearsal](cold-restore.md) now covers a stopped local
fixture namespace, full scratch, exact object metadata and retired markers.

## Run and exact rehearsal checks

```sh
scripts/smoke.sh offline-restore
```

The no-argument wrapper builds actual `fry`, `fry-member` and `fry-admin`, then
runs `python3 -m scripts.smoke.offline_restore` with bounded disposable resources. Source
review, syntax checks and the coordinated runtime run passed. The following
describes the assertions exercised by that successful run.

1. Start fresh PostgreSQL and two authenticated mTLS member processes. Receive
   three exact originals into INBOX; set Seen/Flagged on one. Create Archive and
   APPEND a fourth exact original with Seen and a fixed 2020 internal date. Create
   a dynamic SMTP credential, then replace its stale sender policy and record
   its persisted version. Queue a fifth original for a bounded synthetic relay.
2. The relay returns 451 at MAIL before accepting DATA. Wait for durable queued
   work with a cleared claim lease and acknowledged 451 before its next retry;
   no lease or authority row is manually rewritten. Confirm no relay handoff.
3. Stop the coordinator and both members, wait for actual process exit, and
   assert both member generations are closed and no other client database
   sessions remain. Capture logical hashes of all public `fry_*` tables and
   sequence state. Dump the entire database with same-version `pg_dump` custom
   format; copy both complete member roots and frozen creation configuration.
4. Inventory every archived file and directory, including type/mode and file
   length/SHA-256. Verify copied member inventory equals the stopped source.
   Exercise two negative archive copies: missing database dump and corrupted
   member bytes must fail manifest verification. Preserve the valid archive.
5. Remove the original PostgreSQL container and original member roots; verify
   source processes exited. Start a different fresh PostgreSQL of exactly the
   recorded server version. Verify archive again, then `pg_restore` with
   `--exit-on-error --single-transaction`, without FRY `-init`. Require every
   logical table/sequence hash to match **before** application startup.
6. Copy complete member roots into new locations, verify restored inventory,
   and reopen normally with preserved IDs/incarnations/catalog. No epoch or
   ownership manipulation is allowed; normal startup increments closed owner
   generations. Independently stop one member at a time: through each sole
   survivor, verify both IMAP folder snapshots and download **all five** saved
   receipt originals with the audited administrative CLI. This prevents replica
   fallback from concealing one missing restored copy. Restart normally between
   checks and reconnect the coordinator to the current member generations.
7. Require INBOX's three messages and Archive's one message to retain exact
   folder identity, UIDVALIDITY, UIDNext, UIDs, flags, internal dates and bytes.
   Require restored account scopes/version to equal the saved row. Enable the
   same synthetic relay endpoint and verify exactly one unchanged queued
   handoff. Fresh SMTP AUTH with the retained secret must succeed without a
   bootstrap account; stale sender MAIL must return 553 and current sender MAIL
   must return 250.
8. Receive a new original, verify unchanged INBOX UIDVALIDITY and next-UID
   continuity, then recheck Archive's exact dated Seen original. Stop restored
   processes and assert no unclosed member owners or read pins. Checked cleanup
   precedes final success JSON. The intended report enumerates five saved
   originals, one new original, two independently checked hot replicas, two
   rejected archive variants, one restored handoff, and binary/script/schema
   hashes. These counts match the final successful report.

The manifest covers the database dump, complete member roots and frozen creation
configuration, including directory modes. **Environment secrets and private CA,
certificate and key fixture material are retained separately** inside the overall
private temporary rehearsal directory. They are not recovered from the manifest.
This is not a credential/certificate disaster-recovery, external KMS or secret-
backup test. No rebuilt-binary recovery claim is made; the same built binaries
are used and their hashes are reported. All endpoints and relay data are local
and synthetic; original and restored member roots have private permissions.

The [member ownership contract](replica-transport.md#process-ownership) requires
clean closure before reopening. Advisory locks in a cloned database **do not
fence the old deployment**: the clone has an independent lock authority. The
original installation must remain stopped and isolated. Never run original and
restored installations simultaneously with copied identities or external relay
destinations. This fixture removes its original authority/roots to enforce that
boundary; it does not supply production trusted fencing.

## Security and production blockers

Restoring a snapshot rolls authority back: password rotations, sender-grant or
credential revocations, disabled accounts, reference release and deletions after
the backup may disappear. Old credentials or previously deleted content can
return. A production restore therefore needs an explicit policy and mechanism
for reconciling post-snapshot revocations/deletions before exposing listeners.
An old backup is not a neutral replacement of current authorization state.

A production full backup additionally requires cold-pack/object coverage,
consistent immutable object identities/manifests, trusted fencing of the old
installation and all external writers, secure backup encryption/access controls,
and tested restore/reconciliation across every storage tier. Backup retention,
erasure and legal-hold policy are not settled here; active-storage cleanup does
not imply snapshot/WAL/backup erasure. See [storage lifecycle](storage-lifecycle.md)
and ADR[0008](../adr/0008-retain-originals-independently-of-inboxes.md).

This local hot-only exercise establishes no online backup, crash recovery,
forced owner takeover, availability-zone recovery, RPO0, production RTO, accepted-
mail-loss guarantee or long-term backup policy. Stop-the-world consistency and
same-version restore are deliberate scope limits, not a production HA design.


## Artifact identities and validation boundary

The final report recorded these exact identities:

| Artifact | SHA-256 |
| --- | --- |
| `internal/admin/schema.sql` | `8d71e7e84594854df77f80cc8282122bde6dd4380051ecf2968d5bc2fc4f699a` |
| `internal/receiptstore/schema.sql` | `b62b4b2a6d3ec9b378bed339826eefb76a95ea9c99a1728c6ce48b31b9320c22` |
| [scripts/smoke.sh offline-restore](../../scripts/smoke.sh) | `162347fd89d76f772f4b3d09c73f6535175a82843259b7fbe3bf3bda2c6897b1` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `267caf34b20f462dfce179f03d177c9d4c5550d08b65caf2512865509667b913` |
| `fry` | `efff4aa9fcf92fa9804c33fef948260209651f6697b717995404845794e7a19a` |
| `fry-admin` | `b3508ac88f808ab912afcb3d87e026a129639e5b528f2c3b285184c35a0d299d` |
| `fry-member` | `c546ef426785fe23e449a1d128771399dccdc817f76e4f79671e0e5841a0c9c7` |

An initial harness assertion compared one IMAP date spelling literally; the
fixture was corrected to compare semantic date/time values. This was a test
interpretation issue, not a product restore fix. Only rehearsal scripts/docs
changed after the previously passing full PostgreSQL race suite and vet, so no
new full Go suite was run for this scripts-only correction. The final rehearsal
passed with preserved authoritative IDs/epochs, not manufactured owner closure.
The reported hashes identify the same built binaries used for backup and restore;
they are not evidence of reproducible builds or secret reconstruction.


## Follow-up with storage accounting

The unchanged rehearsal passed again after accounting integration (run 53427,
exit 0), with evidence at `/tmp/fry-offline-restore-accounting.log`. It again
verified five saved originals independently through both replicas, one new
original, one restored queued handoff and two rejected archive variants, with
10 files/five directories and PostgreSQL 18.6. Checked cleanup removed all resources
owned by this run; the two pre-existing unrelated Docker resources were untouched.

The full table/sequence checkpoint now includes populated retained-original
usage counters. Quota policies and alerts remained empty defaults: this is
**not** evidence that configured quota policies or active alerts were restored.
The historical run and hashes above remain valid evidence of their earlier
source snapshot. Follow-up identities are:

| Artifact | SHA-256 |
| --- | --- |
| `internal/admin/schema.sql` | `deaf7bec583d817d8ef2d09f0d75a9c20f09d5c4359dfa3b63d3aa81ebad343a` |
| `internal/receiptstore/schema.sql` | `c52b6cf58942b5f95c347e95dac0a1cade423d9bfe9ad4de0de28fb07d713f07` |
| [scripts/smoke.sh offline-restore](../../scripts/smoke.sh) | `162347fd89d76f772f4b3d09c73f6535175a82843259b7fbe3bf3bda2c6897b1` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `267caf34b20f462dfce179f03d177c9d4c5550d08b65caf2512865509667b913` |
| `fry` | `572ea148eaa2f384cc306ea390e5f7adde3e3165d959aee0ba78b6647ccabfd1` |
| `fry-admin` | `24004117cdcd18cba8d5ee8841d12027ae6a830aec98a30a6f7ff0b20e637221` |
| `fry-member` | `b6c1a6d2bd88a0b3fbd8fca006d7fb53c3ceae6ffd2f3bfd23c62571bd783353` |

All hot-only, offline, original-deployment fencing and separate-secret recovery
limitations above remain unchanged. See [storage quotas](storage-quotas.md) for
accounting semantics and the distinct policy/process validation.

## Follow-up with member-file catalog

Run 34345 passed (exit 0), recorded at
`/tmp/fry-offline-restore-member-catalog.log`, with PostgreSQL 18.6. It verified
all five backed-up originals independently through both hot replicas, one new
original, one restored queued handoff and two rejected archive variants. The
backup contained 10 files and five directories; cleanup completed before the
success report.

The pre-start full-table and sequence comparison now includes the restored
current member-file catalog rows. This fixture did not create historical rewrite
tombstones. Quota policies remained unconfigured, so this does not extend the
claim to configured quota policies or alerts. All existing hot-only/offline and
original-deployment fencing limitations still apply. Artifact identities are:

| Artifact | SHA-256 |
| --- | --- |
| `fry` | `573b0271e889e696bf18932682812fc435a5d4486b8b196d4a71e6c57570b76d` |
| `fry-admin` | `24004117cdcd18cba8d5ee8841d12027ae6a830aec98a30a6f7ff0b20e637221` |
| `fry-member` | `3983074f6e922e2ba8d7bc7eb802bc21b0a864ebab5ed444cdba34adaeffec47` |
| `internal/admin/schema.sql` | `deaf7bec583d817d8ef2d09f0d75a9c20f09d5c4359dfa3b63d3aa81ebad343a` |
| `internal/receiptstore/schema.sql` | `631d4ebbfecbf7493ea67ffe7dd643274c285c487642784ec5fea9ffdf6d2537` |
| [scripts/smoke.sh offline-restore](../../scripts/smoke.sh) | `162347fd89d76f772f4b3d09c73f6535175a82843259b7fbe3bf3bda2c6897b1` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `267caf34b20f462dfce179f03d177c9d4c5550d08b65caf2512865509667b913` |

## Follow-up with bounded member admission

Run 32017 passed (exit 0), recorded at `/tmp/fry-offline-restore-bounded.log`.
The fixture now configures both actual `fry-member` processes with 128 records,
16 MiB per file, 64 MiB member capacity, eight append slots and a 1 MiB maximum
message. It verified all five saved originals independently through each replica,
one new accepted original, one queued handoff, both negative archive cases, and
checked resource cleanup. The backup again contained 10 files and five directories.

The complete pre-start database/sequence comparison includes enabled frozen
member profiles, settled target reservations and current-file occupancy. Explicit
checks before backup and after accepting new mail require both profiles and
exact catalog reservation sums. This fixture still has no historical rewrite
files, cold packs or configured quota policies. Offline, hot-only, separately
retained secret material and original-deployment fencing limitations remain.

| Artifact | SHA-256 |
| --- | --- |
| `fry` | `8c4996434c49418dc13609b57ce5bb87f781a06269b50834d11e486235e2b5ca` |
| `fry-admin` | `24004117cdcd18cba8d5ee8841d12027ae6a830aec98a30a6f7ff0b20e637221` |
| `fry-member` | `07c8df134b1d0c36de05457bf4a44729506de2fef1270830071eb2fcf927d4a0` |
| `internal/receiptstore/schema.sql` | `7d0f20cfbea9b9e6382ed3d90f096ac3eb0b917ca435ddfe4169c7bc4c855a06` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `4356c84f8a3f77b4fdc79cf69b45ecaadbae31af6a0ba2a7d2d574950629f13b` |


## Capacity-lane follow-up

Run 92010 passed the same complete rehearsal with immutable per-file capacity lanes. The fixture uses a 20 MiB file ceiling (128 records, 64 MiB member budget, 1 MiB maximum message) to derive two lanes per file. It verifies lane/profile persistence and exact per-lane target census before backup and after fresh restored acceptance. [The lane validation record](hot-capacity-lanes.md#offline-restore) contains the report scope and exact binary/schema hashes; this supersedes neither the historical snapshot identities nor the recovery limitations above.


## Sealed-file rotation follow-up

Run 95861 passed after two real mTLS rotations per member, producing two sealed files and one current file on each side. All five saved originals were verified independently from both replicas after destroying and restoring the original resources; new receipt acceptance and the queued synthetic handoff also passed. [Rotation validation](hot-file-rotation.md#process-restore-across-sealed-and-current-files) records the four rotation/status/replay checks, unchanged targets, twelve lane censuses, 18-file/nine-directory archive and exact schema/binary hashes. This extends the hot-only restore scope to sealed files; it does not add online, cold-object or crash-recovery guarantees.

## Sealed reclamation follow-up

The later [sealed-file reclamation validation](sealed-file-reclamation.md) passed real-process restore in session `96397` (`/tmp/fry-offline-restore-sealed-reclaim.log`). Before backup, a sixth synthetic original is actually expunged, its recovery deadline is advanced solely to simulate elapsed time, and the normal expiry worker plus two authenticated sealed-reclaim operations remove both physical copies. Five surviving saved originals are independently verified through both restored replicas, one new original is accepted, and the expired receipt/source bytes remain removed. The archive contains 20 files and 11 directories, including four surviving sealed files and two removed-file histories. This extends the tested hot-file history; all offline, secret-recovery, cold-storage and post-snapshot rollback exclusions above still apply.
