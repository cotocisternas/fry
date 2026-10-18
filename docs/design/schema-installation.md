# Atomic schema installation and compatibility

FRY's production commands now require a complete, recorded installation before
opening mail storage. Previously, initialization committed receipt tables before
administration and mail-directory tables; a later DDL failure left a partial
database that could not be initialized again. The new boundary commits all three
components and their version ledger together. It does not upgrade old prototypes.

`internal/installation` composes the receipt, administration and mail-directory
SQL in dependency order. The neutral `internal/dbschema` package owns the
transaction and compatibility contract. Each module retains its existing fresh
component `Migrate` helper for partial unit fixtures; those helpers create no
installation ledger and are not a production initialization path.

## Initialization

`fry -config CONFIG -init-only` creates version 1 in a fresh selected schema.
`-init` initializes and then starts the application. Both require a fresh schema;
repeating initialization refuses an existing installation without changing it.

Initialization uses `synchronous_commit=on` and a transaction-scoped advisory lock
derived from database and schema identity. Competing initializers serialize:
one can publish the complete installation, while the other sees existing FRY
structures and refuses. Component SQL, seeded rows, the ledger and the recorded
catalog signature commit in one transaction. Failure before commit rolls all of
them back. An uncertain commit response requires checking the result; it is not
evidence that creation failed or permission to delete existing structures.

The singleton `fry_schema_version` row records installation version, a SHA-256
digest of ordered, length-delimited component names and SQL, PostgreSQL major,
and a separate catalog digest. SQL remains embedded by its owning module. Future
schema edits change the artifact digest and deliberately fail compatibility until
an explicit transition is implemented; changing the ledger manually is not an
upgrade procedure.

## Compatibility check

`fry -config CONFIG -check-schema` performs the check and exits before opening
mail storage or listeners. It cannot be combined with initialization. Ordinary
startup in `fry`, `fry-member` and `fry-migrate` performs the same check before
`Storage.Open` or member-directory creation. Configuration and certificate reads
can precede it; `fry-admin` uses HTTP rather than a database connection.

The check runs in a read-only, repeatable-read transaction. It requires exactly
one valid ledger row, current version, matching compiled artifact digest,
matching PostgreSQL major and matching catalog signature. It never repairs,
adopts, drops or upgrades an incompatible installation. Ordinary row changes and
sequence advancement are outside the signature and remain valid operation.

The catalog signature covers FRY-prefixed relations and their column definitions,
defaults, nullability, identity/generated properties, constraints, indexes and
sequence definitions. Attached constraints, indexes and user triggers remain
included even if renamed. It also records FRY-prefixed functions/types and relevant
relation settings such as row-security enablement. OIDs, owners, row contents,
sequence positions and storage/statistics data are excluded. These checks detect
representative structural drift; they are not an inventory of every PostgreSQL
object kind, ACL validation or protection against an administrator rewriting the
ledger. Unused standalone collations/conversions are outside this contract.

Catalog deparser output can contain schema-qualified names and can change between
PostgreSQL major versions. This version supports restoration into the **same
schema name and PostgreSQL major**; schema renaming and major upgrades require
separate work. Catalog definitions are reconstructed using PostgreSQL's
[system information functions](https://www.postgresql.org/docs/18/functions-info.html).

Each command pins its pool configuration to one explicitly named schema, default
`public`, before creating connections. Only a simple lowercase schema identifier
is accepted; fallback lists, `$user`, system namespaces and ambiguous names are
rejected. Dedicated member-owner connections inherit the same setting. The
checker also verifies the effective search path and uses qualified ledger/catalog
access. This does not grant schema privileges or prevent privileged DDL after
startup; deploy into an isolated application schema with controlled DDL access.

Database work uses a ten-second context deadline, shortened by earlier caller
cancellation. Rollback cleanup has a separate five-second context budget.
Catalog processing rejects more than 4,096 entries or 4 MiB of definition text.
This bounds the supported installation, without claiming constant-time database
execution or runtime monitoring of subsequent changes.

## Deployment boundary

Existing unversioned prototype databases are intentionally incompatible with
these new binaries. Leave their existing binaries/data intact; this slice provides
no adoption command. Never manufacture a ledger for them or rerun fresh SQL over
existing mail authority. A future conversion must prove the source layout and
preserve its data explicitly.

This is a prerequisite for versioned upgrades, not an upgrade engine, online DDL
protocol, rolling-version compatibility policy or HA mechanism. Database schema
compatibility is also separate from the member ownership, object namespace,
mailbox migration and storage accounting checks that still run afterward.

See [validation](../validation/schema-installation.md),
[ADR 0022](../adr/0022-require-an-explicit-schema-installation.md) and the
[cold restore scope](../validation/cold-restore.md).
