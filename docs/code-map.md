# Code map

Use this map to find the owner of a behavior before editing it. The
[glossary](../GLOSSARY.md) defines domain terms; the
[implementation status](implementation-status.md) links behavior to validation
and remaining limits. The [engineering module design](design/engineering-modules.md)
also contains proposed changes, so use the paths below for the current layout.

## Process entry points

- [`cmd/fry`](../cmd/fry/): coordinator configuration, mail/admin listeners,
  dependency construction, workers and shutdown.
- [`cmd/fry-member`](../cmd/fry-member/): standalone hot-storage member process.
- [`cmd/fry-admin`](../cmd/fry-admin/): administration client.
- [`cmd/fry-migrate`](../cmd/fry-migrate/): operator tool for staged history import
  and cutover, using direct database and private-replica access.

## Durable mail and storage

- [`internal/receiptstore`](../internal/receiptstore/): PostgreSQL authority for
  acceptance attempts, receipts, destinations, references and work; mailbox state,
  outbound queue state, migration state, organization sender grants, packing and
  storage maintenance. Its `member_grpc*` and `remote_grpc*` files implement the
  member gRPC handlers and clients, including authentication and read ownership.
- [`internal/hotstore`](../internal/hotstore/): bounded local byte storage,
  checksummed append records, verified reads and conservative recovery.
- [`internal/objectstore`](../internal/objectstore/): S3 adapter for immutable
  packs, conditional publication and retirement markers.
- [`internal/replicastream`](../internal/replicastream/): bounded replica-stream
  framing, integrity checks and explicit completion semantics.
- [`internal/replicaconfig`](../internal/replicaconfig/): member configuration,
  identity/trust validation and opening local or remote storage adapters.
- [`internal/messageindex`](../internal/messageindex/): bounded MIME structure,
  metadata and exact encoded attachment spans.

Read the [storage protocol](design/storage-protocol.md) before changing durable
outcomes or ownership. SMTP reception streams through receiptstore to its member
adapters. Final success requires the receipt acceptance sequence, including durable
copies and metadata publication. Mailbox publication subsequently makes accepted
mail visible to IMAP. A successful local write or RPC is insufficient on its own.
See [receipt validation](validation/replicated-receipts.md) and the
[member RPC reference](reference/member-rpc.md) for implemented behavior.

## Mail protocols and source workflows

- [`internal/smtpserver`](../internal/smtpserver/) and
  [`internal/imapserver`](../internal/imapserver/): SMTP/IMAP adapters that use the
  durable mail operations; see the [SMTP](validation/smtp-reception.md) and
  [mailbox/IMAP](validation/mailbox-imap.md) records for supported behavior.
- [`internal/outbound`](../internal/outbound/): durable-queue worker, relay
  handoff, retries and failure notices.
- [`internal/imapcollect`](../internal/imapcollect/): external mailbox collection,
  source deduplication, folder mapping and optional source cleanup.
- [`internal/imapmigrate`](../internal/imapmigrate/) and
  [`internal/imapsource`](../internal/imapsource/): staged migration orchestration
  and the source IMAP adapter. See [migration](validation/mailbox-migration.md).

## Administration and shared infrastructure

- [`internal/admin`](../internal/admin/): administration operations, scoped
  authority, auditing and transaction composition.
- [`internal/admintransport`](../internal/admintransport/): Connect handlers,
  legacy HTTP adapters, local/OIDC session handling, original downloads and
  embedded browser assets. The implemented contract is in the
  [administration RPC reference](reference/admin-rpc.md).
- [`internal/maildirectory`](../internal/maildirectory/): incoming domains,
  aliases, mail accounts and submission-account policy. Organization sender grants
  belong to receiptstore.
- [`internal/credentials`](../internal/credentials/): password primitives.
- [`internal/installation`](../internal/installation/) and
  [`internal/dbschema`](../internal/dbschema/): atomic schema installation and
  compatibility checks before storage opens; see
  [schema installation](design/schema-installation.md).
- [`internal/telemetry`](../internal/telemetry/): explicit process telemetry setup.

Keep generated query types within their owning module. Administration transports
call application operations; authorization, mutation and audit remain composed
within their existing transaction. See [contributing](../CONTRIBUTING.md) for
context, error and resource ownership principles.

## Source and generated output

- [`db/schema`](../db/schema/) contains the schemas used by both installation and
  sqlc; [`db/queries`](../db/queries/) contains query inputs.
  [`sqlc.yaml`](../sqlc.yaml) owns generation into the admin, maildirectory and
  receiptstore `dbgen` directories. Use `make sqlc`; do not edit output by hand.
- [`proto/admin`](../proto/admin/) and [`proto/member`](../proto/member/) own the
  protobuf inputs; [`gen/admin`](../gen/admin/) and [`gen/member`](../gen/member/)
  hold generated Go/TypeScript clients. Use the
  [RPC tooling workflow](../tooling/rpc/README.md) for generation.
- [`internal/admintransport/browser`](../internal/admintransport/browser/)
  contains browser RPC source. Its generated bundle is
  [`web/rpc.js`](../internal/admintransport/web/rpc.js); other browser assets live
  beside it. Use `make generate` and `make generate-check` for reproducibility.
- [`third_party/go-imap`](../third_party/go-imap/) and
  [`third_party/go-smtp`](../third_party/go-smtp/) are patched upstream modules
  with separate suites. Root `./...` does not include their complete tests.

## Tooling and evidence

[`Makefile`](../Makefile) dispatches to the [script launchers](../scripts/README.md)
and the pinned native toolchain in [`mise.toml`](../mise.toml) / [`mise.lock`](../mise.lock).
[`tooling/testdeps`](../tooling/testdeps/) is a separate Go module that builds
dependency images and owns disposable fixture lifetimes;
[`internal/testutil`](../internal/testutil/) contains application test helpers.
See [development tooling](development.md) for check selection and setup.

[`docs/design`](design/) describes architecture and proposals;
[`docs/adr`](adr/) records decisions; [`docs/reference`](reference/) describes
implemented RPC contracts; [`docs/validation`](validation/) records scoped
execution evidence and limits; [`docs/research`](research/) holds investigations
and recommendations. Local `.artifacts` outputs support individual runs and are
not a replacement for regression tests or committed validation records.
