# Schema installation validation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The focused PostgreSQL race run passed (session 78892, dbschema 1.828s). It
exercised the neutral installation boundary against disposable schemas using
real PostgreSQL DDL, transactions and concurrent connections. This run preceded
the additional `information_schema` pool-configuration rejection case; that case
was subsequently included in the passing final suite.

The process rehearsal **passed** (session 46889, exit 0,
`/tmp/fry-schema-startup-runtime.log`): nine incompatible startups were rejected
across three restored clones, all reported preservation checks passed, and two
exact originals were verified. PostgreSQL was
`18.6 (Debian 18.6-1.pgdg13+2)`. The final full PostgreSQL race suite and
vet **passed** (session 44087, exit 0): dbschema 1.920s, admin 78.100s,
receiptstore 101.910s, IMAP 68.131s and SMTP 45.936s. Logs:
`/tmp/fry-schema-race.log` and `/tmp/fry-schema-vet.log` (empty successful vet
output).
The implementation contract is described
in [schema installation](../design/schema-installation.md) and
[ADR 0022](../adr/0022-require-an-explicit-schema-installation.md).

## Independent component tests

`internal/dbschema/schema_independent_test.go` uses a fresh isolated schema per
case, with synthetic components representing receipt, administration and
directory dependencies. They include a serial sequence, foreign keys, defaults,
check constraints and a unique index. These tests do not manufacture accepted
mail or claim production-scale initialization performance.

| Boundary | Assertion |
| --- | --- |
| Atomic initialization | Invalid SQL in the final component leaves no FRY relation or ledger committed; valid initialization can subsequently succeed. |
| Concurrent initialization | Two simultaneous callers produce exactly one successful initializer and one refusal; the result passes compatibility checking. |
| Fresh-only initialization | Repeated initialization and existing unversioned FRY tables/sequences are rejected. Empty or partial schemas cannot pass compatibility checking. |
| Read-only compatibility | Repeated checks preserve inserted row contents and exact sequence position/called state. Ordinary row updates and sequence advancement remain compatible. |
| Structural drift | Dropped constraint, index or column; changed default or sequence definition; and an extra FRY table each fail checking. |
| Recorded authority | Wrong artifact checksum, catalog digest, PostgreSQL major or version, and missing ledger row/table each fail checking. A changed compiled SQL artifact also fails. |
| Pool configuration | Safe single-schema settings normalize idempotently; fallback lists, `$user`, system namespaces and malformed identifiers are refused. A pool with a runtime fallback search path cannot pass checking. |

The unsupported-version test removes the fixture's version constraint before
writing an invalid version so it reaches the checker, rather than merely proving
PostgreSQL rejects the write. Corrupt fixture changes are never adoption or
upgrade procedures.

## Passed process rehearsal

Run `scripts/smoke.sh schema-startup`. The bounded local harness builds actual
`fry`, `fry-member` and `fry-migrate` binaries, starts a disposable installation,
accepts an exact synthetic original and cleanly closes its writers. The passed run verified these assertions:

1. `fry -check-schema` succeeds without changing authority or member files.
   Repeated `-init-only` refuses existing FRY objects and preserves authority.
2. A complete dump restores into each fresh PostgreSQL clone using the same
   schema name and PostgreSQL version. The untouched copied ledger passes
   checking in the replacement database; no ledger is regenerated.
3. Three independent clone variants remove the ledger, corrupt its artifact
   checksum, or drop a receipt index. All three binaries must reject each variant
   with a schema-compatibility error: nine refused startups in total.
4. Fresh coordinator/member/migration storage roots remain absent after every
   refusal. Existing roots remain byte-for-byte unchanged. Table rows and sequence
   states remain unchanged, and schema-only dumps before/after refusal match.
   The dump comparison strips only PostgreSQL 18's randomly generated
   `\\restrict`/`\\unrestrict` tokens, so a DDL repair attempt is independently
   observable.
5. The untouched original installation reopens, returns the saved original and
   mailbox identity, then accepts another original normally. Final success is
   emitted only after checked process/database and temporary-root cleanup.

The final JSON records the binary and source identities listed below. Success
followed checked cleanup.

## Scope and remaining gates

The ledger records version 1, the exact ordered component artifact digest,
PostgreSQL major and a bounded catalog signature. It does not record mutable
rows or sequence positions. Compatibility requires the **same schema name and
PostgreSQL major** on restore; cross-schema renaming and major upgrades are not
supported by this slice. The real clone rehearsal, rather than equality between
differently named synthetic schemas, tests recovery into a replacement database.
The harness does not separately assert that numerical catalog OIDs changed.

Checks use a read-only repeatable-read transaction and do not adopt, repair or
upgrade old prototypes. Existing module-level `Migrate` helpers remain useful for
partial unit fixtures but create no production installation ledger. An
unversioned database must retain its compatible binaries until an explicit,
validated conversion exists; manually adding or editing the ledger is unsafe.

This is not a general PostgreSQL security audit: ACLs, every possible object kind
and hostile superuser edits are outside the catalog contract. It checks startup
compatibility, not continuous DDL monitoring, online upgrades, rolling-version
operation, backup completeness or deployment fencing. Existing member ownership,
object namespace and storage lifecycle checks still apply independently.

## Runtime identities

| Binary | SHA-256 |
| --- | --- |
| fry | `881d4515bcf394ac2454cbf42a5d839fb2460b3f1926af57f126f2ceaf0a892a` |
| fry-member | `0da7c3aa6be8d8cc822a2ac9904847f573bdd874e09582e00465f7efa5bfe75b` |
| fry-migrate | `f908e6a799d86b0a8baac1fc29ad84dabeb8c07c2b45218714f1856e3eb55f89` |

| Source | SHA-256 |
| --- | --- |
| cmd/fry-member/main.go | `3cedf355e44d13d65e87e27c97bcea5df2736c517c74ae496df56d6e022dc431` |
| cmd/fry-migrate/main.go | `ebe08136f0c3eee313ce60b2560785ffa57ec084bf1e8db14923cd9857c31c45` |
| cmd/fry/main.go | `fef96d5b60f65e86eaab886234f3c071faa950e01c25da3ed26e0ea7626cfada` |
| internal/admin/schema.sql | `deaf7bec583d817d8ef2d09f0d75a9c20f09d5c4359dfa3b63d3aa81ebad343a` |
| internal/dbschema/schema.go | `26889b9f3ca0992aa9605cc715dbe914622eff090c7847afe1952302cdcde25c` |
| internal/installation/schema_linux.go | `2d486ee45d8abbec9e355c2e5d09208673795bf0dfd596449285651312ce1809` |
| internal/maildirectory/schema.sql | `f4021b467fbca8dd5052b7a655250ad5fd37e589658afa6979165c0f6d59b3cb` |
| internal/receiptstore/schema.sql | `dbda2fd47b497ab57f1ce9ea057aa2ce808ca13e1637eff326fae5c44071cd25` |
| [scripts/smoke.sh schema-startup](../../scripts/smoke.sh) | `38869e66e69d4a9312082b3bd068b068d23d02b0bba0fc81eb23a185179364e7` |
| [scripts/smoke/offline_restore.py](../../scripts/smoke/offline_restore.py) | `7fdcb752d4ddadea78e42c114c90f31eba97920cdcb8b696680a8a462c717a3b` |
| [scripts/smoke/schema_startup.py](../../scripts/smoke/schema_startup.py) | `cf7b05e668218c83881b5f71031315a97e2e52fa8c4294eec2f69eb9ece31b76` |

## Cold restore follow-up

The [same-namespace cold restore rehearsal](cold-restore.md#follow-up-with-the-versioned-installation)
also passed with these versioned `fry` and `fry-member` binaries (session 34527,
exit 0, `/tmp/fry-schema-cold-restore.log`). Its exact pre-start table/sequence
comparison included the copied installation ledger. It verified three saved
cold originals with both hot members stopped, one fresh packed original, retained
removal/marker state, two rejected damaged archives and changed-namespace refusal,
then checked cleanup. This adds same-schema-name/same-PostgreSQL-version recovery
evidence without an OID-value or schema-renaming claim.
