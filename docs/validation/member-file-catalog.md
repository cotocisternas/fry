# Member-file catalog validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The implemented [file-catalog design](../design/member-file-catalog.md) and
[ADR 0017](../adr/0017-track-hot-file-lifecycles-explicitly.md) make file selection
and rewrite deletion authority explicit. This preserves the existing whole-file
rewrite, framing, member-wide gates and transport identities. It does not add
segments, reception during rewrite, bounded startup memory or automatic crash
failover.

## Implemented contract

`fry_member_file` binds each globally unique immutable file ID to one member and
member-relative directory. Directory bindings remain unique within a member,
including removed history. A partial unique index permits one current file per
member. `fry_member.incarnation` still means the hotstore `FileID`; member IDs and
process ownership epochs keep their existing meanings.

Initial member and current-file registration commit together with synchronous
commit enabled. Before any mutating hotstore open, read-only preflight checks the
required schema, current catalog/member pointer, actual immutable file header,
and active rewrite/catalog associations. Missing catalog history is not adopted
or backfilled. Header inspection does not scan payloads, sync, create files or
remove staging files. Existing placement and write checks require the exact
current catalog binding.

A rewrite reserves its successor directory in a durable job, creates its header,
and registers the actual successor ID as building before copying any originals.
Publication atomically changes old current to retiring, building to current,
member pointers and surviving target locators. The old handles close before
cleanup verifies the exact job, catalog, directory and header. Durable unlink
precedes the removed tombstone and existing cleanup completion. Removed catalog
rows remain recorded.

Recovery permits a missing registered file only with the exact job/catalog
proof. An interrupted generation-directory removal may leave regular
`writer.lock` or `stage-*` scaffolding; unexpected entries or symlinks block
cleanup. A different header blocks deletion. Uncertain publication retains both
files and freezes unsafe operations until database authority is resolved.

If a successor data file exists before registration becomes authoritative,
recovery preserves it and fails closed, even if it contains only a valid header.
Only an absent or actually empty job-reserved directory can be cleaned without a
registered file. This unresolved window needs explicit reconciliation and can
block availability. Failed initial registration similarly does not authorize
adoption of existing unidentified bytes. Existing schemas require an explicit
upgrade outside this slice; initialization remains fresh-only.

## Focused evidence

The following disposable PostgreSQL 18.6 runs completed with the race detector:

| Run | Coverage | Result |
| --- | --- | --- |
| 35397 | New catalog and read-only identity tests | PASS; receiptstore 4.032s, hotstore 1.035s |
| 33529 | Final catalog/identity regressions, including fresh registration rollback and unregistered-directory symlink refusal | PASS; receiptstore 4.301s, hotstore 1.036s |
| 99104 | Existing hot rewrite, eviction, final cleanup, remote maintenance and accepted-retry tests | PASS; receiptstore 3.876s; all packages compiled |

[Catalog tests](../../internal/receiptstore/member_files_linux_test.go) exercise
initial binding, refusal before filesystem mutation, real publication and exact
survivor bytes, stable process epochs, preparing-write exclusion, and faults
around registration, copy, publication and unlink. They cover registered absent
paths, orphan active rows, unknown replacement entries and conservative
preservation of unregistered data.
[Identity tests](../../internal/hotstore/identity_linux_test.go) check read-only
inspection and malformed/missing files. Existing
[rewrite](../../internal/receiptstore/hot_cleanup_linux_test.go) and
[remote maintenance](../../internal/receiptstore/member_maintenance_linux_test.go)
checks retain their pin, recovery and fencing coverage.

Independent source review found no remaining blocking issue. `go vet ./...` passed. Full race run 65471 failed only the legacy fake-placement
test's required error classification: opening a new member identity over an
existing root now fails preflight with `ErrConflict` before reaching the busy
lock. Its assertion was updated to allow either rejection, require a nil
duplicate and no new database identity, and prove the original still accepts
mail. No executable behavior changed for that correction. Full race rerun 91167 passed across all packages, recorded at
`/tmp/fry-member-file-catalog-race-final.log` (admin 68.353s, receiptstore 57.002s,
IMAP 66.797s, SMTP 41.373s). The isolated acceptance and full SMTP follow-ups below also passed. These results establish
neither production readiness nor a throughput improvement.

## Offline restore follow-up

The [hot-only offline restore rehearsal](offline-restore.md#follow-up-with-member-file-catalog)
passed as run 34345, with evidence at
`/tmp/fry-offline-restore-member-catalog.log`. All five backed-up originals were
verified independently through both replicas, followed by one new original and
one queued handoff; two corrupted/missing archive variants were rejected. Full
pre-start table/sequence equality includes the current catalog rows. Historical
rewrite tombstones and configured quota policies were not present in this
fixture. This is neither cold-object nor online/crash snapshot coverage.

The binaries and schemas actually used were:

| Artifact | SHA-256 |
| --- | --- |
| `fry` | `573b0271e889e696bf18932682812fc435a5d4486b8b196d4a71e6c57570b76d` |
| `fry-admin` | `24004117cdcd18cba8d5ee8841d12027ae6a830aec98a30a6f7ff0b20e637221` |
| `fry-member` | `3983074f6e922e2ba8d7bc7eb802bc21b0a864ebab5ed444cdba34adaeffec47` |
| `internal/admin/schema.sql` | `deaf7bec583d817d8ef2d09f0d75a9c20f09d5c4359dfa3b63d3aa81ebad343a` |
| `internal/receiptstore/schema.sql` | `631d4ebbfecbf7493ea67ffe7dd643274c285c487642784ec5fea9ffdf6d2537` |
| [scripts/smoke.sh offline-restore](../../scripts/smoke.sh) | `162347fd89d76f772f4b3d09c73f6535175a82843259b7fbe3bf3bda2c6897b1` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `267caf34b20f462dfce179f03d177c9d4c5550d08b65caf2512865509667b913` |

## Isolated acceptance follow-up

Run 67079 passed: `/tmp/fry-acceptance-member-catalog-1/baseline/report.json`
and its checked-cleanup `run.json`. All 1,024 receipt statuses and full originals
were verified; 793 headers had optional eager metadata. The acceptance interval
was 969.053725ms (1,056.700958 originals/s), with p50/p95/p99 of
14.450193/20.808120/28.426401ms.

This report explicitly records the actual member filesystem as **tmpfs**;
PostgreSQL storage is separate. It is not persistent hot-disk fsync evidence.
The earlier fast accounting repeat measured 1,127.32/s and p99 25.38ms; the
unexplained 208/s outlier remains unchanged. These few short samples establish
neither a speedup nor a regression bound. The separate 5,000-message disk-backed
SMTP follow-up below also passed.

Report SHA-256:
`16a0c948c4e172aa09f277f53db70a4bc6d1403432ffe869ad3d90160bf74494`.
Relevant source fingerprints recorded in that report are:

| Source | SHA-256 |
| --- | --- |
| `schema.sql` | `631d4ebbfecbf7493ea67ffe7dd643274c285c487642784ec5fea9ffdf6d2537` |
| `member_files_linux.go` | `d6d649ce45de6f1d9e0549dff5827b069a84be26f26e6ebe83480da412294486` |
| `member_linux.go` | `3175de408f23e10171c68032af637976af6e06832f3f77b6aa1796ce48cb34c7` |
| `postgres.go` | `3e3171bc2ad7cc529e2835d2eeb0aeb9290f258d0d014f7e5a6b529c75302284` |
| `transport_linux.go` | `d60ca09c9f813f94c6d0334d4eb4aa2c8b3c264e626e90a524d70b491c51b521` |
| `../hotstore/identity_linux.go` | `e654269c6f229a383fb27964837aef5efc39e5249e6ed57164eb8abfb1e6b866` |
| `acceptance_measurement_linux_test.go` | `96580b0f70f4deb4f83ad8ffe44cc68e5442a7129502f86a27a072bd13d0114a` |
| `scripts/bench/acceptance.sh` (removed wrapper) | `88aec1cec3cc5f592f278c616cbe96a20ff06718d0d159d5911680845132ca9a` |

## Full SMTP follow-up

Run 59859 passed on Btrfs with the actual FRY process, one local member pair,
16 workers and 5,000 synthetic 65,536-byte originals. All 5,000 were accepted;
none deferred or failed. SMTP elapsed was 11.984073777s (417.220395/s), with
p50/p95/p99 of 38.143837/46.528262/51.630313ms. Last acceptance to all visible
was 2,351.628065ms. The run verified all 5,000 headers and eight full/partial
body samples, **not all 5,000 bodies**. This differs from the isolated test's
complete body verification.

Cold header/date/unseen search took 14,271.325316ms; warm search took
189.374761ms. Initial cold search remains a known limit, without attribution to
accounting or the catalog. FRY peak RSS was 46,908KiB, excluding PostgreSQL and
the load client. The earlier accounting run's 445.656/s and p99 50.086ms are
short local observations, not an overhead bound or evidence of a speedup.

Evidence: `/tmp/fry-load-5000-member-catalog.json` and its log; report SHA-256
`3c9937484598864f11b7062704573730df2868de8ea5925a072e90dc841a5843`.
Fixture cleanup was checked; only two pre-existing unrelated Docker containers
remained. This completes this slice's focused, integrated, restore and local
measurement gates, without establishing production capacity, segmentation or
automatic crash recovery.
