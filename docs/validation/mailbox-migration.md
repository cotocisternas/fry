# Staged mailbox-history migration

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The experimental Go implementation imports history through a real read-only IMAP session. It preserves complete raw messages, INTERNALDATE, persistent flags/keywords, selectable folders, and subscriptions to those folders. Different source UIDs remain different memberships even when their bytes and Message-ID match. FRY assigns new destination UIDs; client checkpoint reset remains part of cutover.

This is separate from ongoing collection. Collection deliberately ignores later source changes. Migration performs successive complete inventory passes and reconciles state inside an isolated destination before activation. [Protocol research](../research/imap-migration-contract.md) records the underlying guarantees and limits.

## Modules and state

- `internal/imapsource.Reader` authenticates with verified TLS, reads folder/subscription inventories, issues finite UID-window searches, validates metadata, and streams bodies. Explicit plaintext is restricted to numeric loopback addresses. A body is successful only after its UID, literal length, and tagged command completion are verified. Source reads use PEEK and never STORE, EXPUNGE, or delete source mail.
- `internal/imapmigrate` coordinates bounded runs and resumes. Each pass captures a complete source folder/UIDVALIDITY/UIDNEXT inventory; UIDNEXT bounds arrivals, not flags or expunges. A new pass fences earlier workers and rescans metadata while retaining accepted copies. A resumed pass continues its durable window cursors.
- `receiptstore` owns the job, generations, attempts, memberships, reconciliation, audit events, and activation. Receipt acceptance, destination placement, and the migration ledger commit together. Retrying an ambiguous accepted attempt does not append another message.

Creation requires a new inbox identity. Existing inboxes and identities with historical accepted destinations are rejected. Migration reservation takes an exclusive transaction advisory lock on the inbox's admission key; ordinary ingress takes a shared lock on that same key in both preparation and final acceptance. This covers reservation races before an inbox row exists while allowing normal deliveries to overlap. Existing row guards and migration-generation checks remain in force. Reserved inboxes are inaccessible to ordinary mailbox operations and incoming acceptance. Administrative receipt-content access retains its existing separate authorization/audit requirements.

Every import checks its job generation during preparation and final publication. Final-pass mutations recheck their deadline immediately before committing. Superseded unaccepted attempts still require actual member cancellation; a generation fence never proves that disk I/O has closed.

A pass advances a window only after every retained observed UID has durable mail. Retrying an interrupted background window also invalidates entries now absent from that window; it cannot retain a previously copied ghost or leave a vanished pending copy blocking completion. The same contradiction fails a final pass. Only a completed pass can remove stale memberships, in bounded reconciliation batches. Recovery references and physical cleanup obligations remain intact.

After reconciliation, FRY rechecks the source namespace and arrival watermarks and streams a manifest over source identities, destination UIDs, flags, dates, sizes, and receipt content digests. This verifies stored metadata against the completed ledger; it is not a new download of every payload. A final pass moves to `ready`. Activation atomically removes the staging gate and permanently stops migration writes. Source barriers, traffic switching, and client reset are external operational steps.

## Operator command

`cmd/fry-migrate` uses direct database and private-replica credentials and is an operator-only tool. It is not yet exposed through the web administration interface. Its JSON configuration embeds the same storage configuration as `cmd/fry`. Passwords come from the named environment variable; they are not command-line arguments or logged configuration.

For an online destination, configure the two remote member URLs, expected identities/domains, pinned server keys, and coordinator mTLS credentials as in [replica validation](replica-transport.md). A migration process must not open local member files already held by a running coordinator. [The local example](../../examples/local-migration.json) instead uses separate target member identities/directories for a disposable or offline exercise. Use a fresh dedicated initialized schema; these prototype schema changes do not upgrade an existing installation.

```sh
go build -o /tmp/fry-migrate ./cmd/fry-migrate
# Set FRY_DATABASE_URL and FRY_MIGRATION_SOURCE_PASSWORD privately in the environment.
/tmp/fry-migrate -config migration.json -action begin
/tmp/fry-migrate -config migration.json -action resume
/tmp/fry-migrate -config migration.json -action status
```

Repeat `resume` until `Run.Complete` is true. `begin` starts a new generation and background pass; use it to reconcile subsequent source changes. `resume` continues the current pass. A failed source consistency check requires investigating the change and starting another pass, not silently accepting a changed identity. JSON output distinguishes the durable phase, generation, copied memberships, remaining folders, pending copies, and this run's bounded work.

Before the final pass, establish the external write and admission barrier and record an attestation file with these exact keys:

```json
{
  "actor": "operator identity",
  "reason": "approved cutover reference",
  "barrier": "how all source clients, delivery paths, and administrative writers were paused",
  "arrival_handoff": "where delayed/new arrivals are queued and how they reach FRY",
  "started_at": "actual UTC time when the first affected service was paused, in RFC3339 format"
}
```

The timestamp must be real and no older than 30 minutes. FRY records this as an operator assertion; repeated identical IMAP scans cannot prove that another writer is stopped.

```sh
/tmp/fry-migrate -config migration.json -action begin-final -freeze freeze.json
# Resume bounded runs until the phase is ready, within the same deadline.
/tmp/fry-migrate -config migration.json -action resume
/tmp/fry-migrate -config migration.json -action activate -generation 2
```

Use the actual generation reported by this final pass. An incorrect generation or expired deadline prevents activation. Activation retries are idempotent after success. Configure the destination's credentials/addresses and switch traffic using the planned cutover procedure. Do not route new traffic back to the old system after activation without reconciling new destination state.

## Evidence

```sh
./scripts/tests/receipts.sh -race -run '^TestMigration'
go test -race ./internal/imapsource
go -C third_party/go-imap test -race ./...
./scripts/smoke.sh replicas
```

PostgreSQL tests cover reservation isolation and races, final synchronization, stale worker fencing, accepted-but-lost replies, partial inventories, vanished UIDs in resumed windows, UIDVALIDITY changes, immutable metadata, source preservation, and deadline crossings during publication/activation. Real TCP tests exercise background copying, source flag changes/removals, expunge, append, folder rename/create/delete, empty folders, subscriptions, and bounded restart. The process smoke runs the actual migration CLI against mTLS replica processes alongside the active mail coordinator, then authenticates to the activated mailbox after restart.

Source protocol tests cover trailing UID, tagged failure after the literal, wrong UID/size, truncated/duplicate literals, missing metadata, bounded/out-of-range/duplicate search results, and cancellation. Isolated pinned-library patches supply strict serialized FETCH routing, baseline LSUB support, and sticky literal failure state; [patch provenance](../../third_party/go-imap/FRY-PATCHES.md) documents their scope.

## Limits and remaining rollout work

No real customer mailbox was migrated. The 30-minute service-resumption target requires a representative rehearsal and time reserved for traffic switching or aborting before activation. Baseline final synchronization scans every message's metadata; a small remaining byte backlog alone does not prove readiness. Deadline checks protect publication decisions, not a distributed wall-clock guarantee about when a networked commit reply reaches the operator.

Each run is bounded by time, messages, and UID windows; each window spans at most 1,024 UID values. Sparse high UID spaces can take many requests. Discovery limits all LIST entries to 4,096; source names must fit FRY's 255-byte folder names. Hierarchy delimiters map to `/`; ambiguous mappings fail. Nonselectable virtual parents are represented by their descendants. Subscriptions to nonexistent/nonselectable folders are currently omitted. Explicit `omit_subscriptions` disables subscription migration; the default uses baseline LIST plus LSUB.

Finite queries bound honest-source cardinality. The pinned protocol parser can still allocate an oversized hostile LIST/SEARCH response before application limits reject it. CONDSTORE/QRESYNC optimizations, adversarial parser allocation bounds, parallel copy scheduling, migration web screens, source-barrier automation, and representative-scale cutover measurements remain future work. Source folder rename and UIDVALIDITY reset may require recopying history. Pending/copy history and audit metadata currently remain in PostgreSQL; no ledger-retention policy is implemented.
