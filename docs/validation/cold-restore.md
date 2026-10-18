# Offline cold-pack restore rehearsal

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The final disposable local runtime **passed on 2026-10-17** (session 44822,
exit 0): three saved originals were read from restored cold packs with both hot
members stopped, one fresh original subsequently packed and read successfully,
and one previously expunged original remained removed. The run preserved one
retired object marker and rejected two incomplete/corrupt archives. Evidence:
`/tmp/fry-cold-restore-final.log` (machine-local artifact). PostgreSQL source and
restored target both reported `18.6 (Debian 18.6-1.pgdg13+2)`; the archive
contained 14 files, including frozen configuration. All reported checks passed,
and success followed checked cleanup. An earlier run (27921) also passed before
these configuration, version and source-identity checks were added.

This is a quiescent, same-namespace recovery exercise against a deliberately
narrow local HTTP object fixture. It establishes neither AWS S3 behavior nor a
production backup, availability, RPO or RTO guarantee. It complements the
[hot-only restore rehearsal](offline-restore.md).

## Recovery unit and procedure

Run `scripts/smoke.sh cold-restore` to build the actual binaries and execute
`python3 -m scripts.smoke.cold_restore`. Every process, database, object endpoint and
filesystem root belongs to the disposable rehearsal. Success is printed only
after checked process/database cleanup and removal of temporary backup roots.

The recovery unit includes the full PostgreSQL authority, every member root,
the complete pack scratch directory, and every object in the fixture's saved
namespace. Restoring just database rows or published pack bodies is insufficient:
retired markers and their exact opaque ETags also matter.

The final passed run exercised these checks:

1. Receive three synthetic originals and APPEND another into Archive with a
   fixed internal date and Flagged state. Set Seen on an INBOX original. Allow
   normal packing and hot reclamation to remove both hot copies of all four.
2. Expunge one original through IMAP. Move only its recovery-reference expiry
   into the past to simulate elapsed time; normal expiry, compaction and object
   retirement must then establish its removed state. No accepted rows or object
   removal authority are fabricated.
3. Stop the coordinator and both member processes cleanly. Check all member
   ownership generations closed, no parent/read closure debt, no application
   database sessions, and no unfinished pack construction/retirement.
4. Close the object HTTP fixture before exporting state. Capture a full custom
   PostgreSQL dump, every member root and complete scratch tree. Archive frozen coordinator/member configuration and hash the exact
   archive file set, lengths, SHA-256 values and directory/file modes. Reject
   copied archives with a missing object-state file or corrupted object-state
   bytes before target startup.
5. Remove the original database, member roots, scratch tree and source object
   state. Restore into a fresh database without initialization or editing owner
   epochs, IDs or object namespace authority. Compare all public `fry_*` table
   contents and sequence states before starting the replacement processes;
   compare restored member and scratch trees against the archive manifest.
   Check the restored PostgreSQL version and load the archived configuration,
   changing only member-root and scratch paths for the replacement files.
6. Restore the object fixture at the **same numeric loopback endpoint**. Preserve
   every key, exact body, digest, quoted opaque ETag, retired flag, configuration
   and serial counter. A reachable fixture at a different endpoint must fail
   FRY's immutable object namespace check; no namespace rebinding is performed.
7. Stop both restored hot members and read all three retained originals through
   IMAP, checking exact bodies plus folder UIDs, UIDVALIDITY, UIDNEXT, flags and
   internal dates against the saved snapshots. Confirm the expunged original is
   still removed. Restart the members, accept and pack one fresh original, and
   verify the old object entries remain identical while new objects and the
   opaque ETag serial advance.

## Fixture persistence evidence

The final focused race run passed (session 45105,
`/tmp/fry-s3-fixture-state-final.log`): helper `1.019s`, command `1.010s`.
Command tests cover clean cancellation/export, export failure without a false
success message or overwritten archive, and malformed-state startup refusal.
`go vet ./...` passed (`/tmp/fry-cold-restore-vet.log`). Production storage code
did not change in this slice; the preceding full PostgreSQL race suite is
recorded in [member-storage diagnostics](member-storage-diagnostics.md).

`TestSnapshot*` uses real HTTP conditional PUT, HEAD and range GET requests to
prove restored bodies, checksums, opaque ETags and future serial progression.
It checks that an existing retired marker rejects a producer's late
`If-None-Match: *` publication. This fixture does not model distinct IAM
identities or prohibit an otherwise valid conditional overwrite by another
identity.

Exports require HTTP closure and refuse to overwrite an existing archive.
Imports validate the bounded versioned format before binding a listener:
64 MiB encoded input, 32 MiB aggregate bodies and at most 128 objects. Tests
reject live export, missing/truncated/trailing/oversized data, unsupported
version, missing required fields, duplicate keys, wrong digests, invalid ETags
or serials, unsafe keys, null bodies and invalid retirement markers. Listener
addresses must be canonical numeric loopback addresses; restore requires the
saved fixed nonzero endpoint without a random-port fallback. This is a fixture
snapshot format, not an S3 versioning or backup standard.

## Limits and operational gates

All writers were stopped before capture. A cloned database's owner locks do
**not** fence an old installation: the original deployment must remain stopped
or be fenced independently before any replacement starts. This run destroys
its original authority and bytes rather than trusting copied lock rows.

TLS certificates/private keys and admin, IMAP and SMTP environment secrets
remain separately available in the private rehearsal directory. They are
outside the backup manifest; this is not credential, KMS or secret disaster
recovery. Frozen configuration, including synthetic setup values, is included in
the private archive; external environment secrets and TLS material remain
outside it.

An older snapshot can restore credentials or grants revoked later and resurrect
mail deleted after capture. Backup retention, deletion propagation and restored
authority reconciliation require a production policy. No online or crash backup,
concurrent mutation, cross-account object copy, AZ failure, AWS IAM/lifecycle/
consistency behavior, production fencing or measured recovery-time claim is
covered. Production recovery still requires a trustworthy deployment fence,
complete durable object/metadata capture, secret recovery, tested retention and
larger failure/recovery exercises.

## Final passed run identities

The final runtime report preserves these binary SHA-256 values:

| Binary | SHA-256 |
| --- | --- |
| fry | `d850259f32267adab1afa683d39be725b0ff1d10f25ad1101be926be466ba4c9` |
| fry-admin | `ca860183a937c3596608185077db962bd64d0362607d45d16ffd5a76ce8677ad` |
| fry-member | `e9a5f6903431a286f24bc04f8e926c8abdf308e1f28c455a46aaca0125b41505` |
| fry-s3-fixture | `32541357a320b1f0ff2a0696f5d51c1bc42effd93b61b0f195c469b909f598a7` |

Reported source SHA-256 values:

| Source | SHA-256 |
| --- | --- |
| examples/local-admin.json | `c4f5f8e0a1c231f7ca39698706d9ded0586faeb01782a4f67c5236559571b90c` |
| internal/admin/schema.sql | `deaf7bec583d817d8ef2d09f0d75a9c20f09d5c4359dfa3b63d3aa81ebad343a` |
| internal/receiptstore/schema.sql | `dbda2fd47b497ab57f1ce9ea057aa2ce808ca13e1637eff326fae5c44071cd25` |
| internal/testutil/cmd/s3fixture/main.go | `aace24e43e8f05ecdf007c60a1f8d3c20f206f2d2419fb696a7e9e28a8cecc90` |
| internal/testutil/s3fixture/server.go | `209de278eeb2aeaa720d8203832104c162f19643fc689809571e928a2b36195b` |
| internal/testutil/s3fixture/state.go | `114a246c5252befbde75d6617239cdc11c7e3cfaea43eb8f5a20da27338f174e` |
| [scripts/smoke.sh cold-restore](../../scripts/smoke.sh) | `0e2cd4d14a9c30544dad3431cd29ed509e967a07c7fd19f25b17c076749faa9b` |
| [scripts/smoke/cold_restore.py](../../scripts/smoke/cold_restore.py) | `1b74ab79703a395674eb45ee00283d768e028778c75aaa647a3ff2f462f35eb6` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `7fdcb752d4ddadea78e42c114c90f31eba97920cdcb8b696680a8a462c717a3b` |

## Follow-up with the versioned installation

The same rehearsal **passed again** with the new versioned startup binaries
(session 34527, exit 0, `/tmp/fry-schema-cold-restore.log`). The 14-file archive
restored three retained cold originals, accepted and packed one fresh original,
preserved one removed original and one retired marker, rejected both damaged
archive variants and a changed namespace, and completed checked cleanup. Cold
reads again succeeded with both hot members stopped.

The exact pre-start table/sequence comparison now includes the copied schema
version ledger. Startup accepted the restored authority without initialization
or ledger modification. This is same-schema-name/same-PostgreSQL-version restore
evidence; the test makes no assertion about particular OID values or renamed
schemas. See [schema installation validation](schema-installation.md).

The changed binary identities were:

| Binary | SHA-256 |
| --- | --- |
| fry | `881d4515bcf394ac2454cbf42a5d839fb2460b3f1926af57f126f2ceaf0a892a` |
| fry-member | `0da7c3aa6be8d8cc822a2ac9904847f573bdd874e09582e00465f7efa5bfe75b` |

The reported admin/fixture binary and cold-harness source identities matched the
preceding passed run. All previously stated fixture, secret-recovery, fencing and
production-recovery limits remain applicable.
