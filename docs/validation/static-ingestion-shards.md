# Static ingestion shards

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The coordinator can distribute new receipt attempts across a fixed catalog of independent replica pairs. Each pair retains the existing two-copy acceptance rule; one pair's maintenance does not require locks on unrelated pairs. This is an implementation experiment on fresh schemas. It does not establish production throughput, automatic failover, repair, online expansion, or independent physical failure domains.

## Configuration

[local-shards.json](../../examples/local-shards.json) is a synthetic two-pair example. Use either the existing top-level `members` pair or `ingest_shards`, never both:

```json
{
  "ingest_shards": [
    {
      "id": "00000000000000000000000000000065",
      "members": [
        {"id": "00000000000000000000000000000001", "domain": "zone-a"},
        {"id": "00000000000000000000000000000002", "domain": "zone-b"}
      ]
    },
    {
      "id": "00000000000000000000000000000066",
      "members": [
        {"id": "00000000000000000000000000000003", "domain": "zone-a"},
        {"id": "00000000000000000000000000000004", "domain": "zone-b"}
      ]
    }
  ]
}
```

This fragment needs the rest of the coordinator configuration. IDs are nonzero 32-digit hexadecimal identifiers. The catalog supports 1–16 shards, with exactly two distinct members and failure domains per shard. Member IDs cannot be shared across shards. Members in different shards may use the same failure-domain labels. Labels describe deployment facts; they do not create physical separation.

Local members use independent roots under `data_directory`, named by member ID. With `replica_tls`, every member instead supplies its authenticated `url` and pinned `server_key` as in the existing remote configuration. Maintenance enumerates all configured members. `member_capacity_bytes` applies to **each** member, so adding pairs without adjusting budgets increases configured capacity. Roots on the same device share its actual capacity and I/O. Include staging and reclamation headroom; these budgets do not measure free space or prove a six-hour outage reserve.

The constructor binds the complete catalog to PostgreSQL. Configuration ordering is irrelevant. Changing shard count, IDs, pair membership, or the placement algorithm requires a future explicit transition. Restarting with a different catalog fails closed. Old authority or member roots must not be silently discarded to make a configuration pass. Existing schemas need an explicit upgrade/conversion procedure, which this prototype does not provide.

New attempts are placed by a versioned hash of the server-generated attempt ID. Placement and exact write targets are durable before byte dispatch. Retries retain those targets; an unavailable pair does not redirect that attempt to another pair. Accepted retries resolve through durable receipt authority without reopening members. Original bytes remain referenced through their stored locators during packing and cleanup.

## Validation

The nine focused shard tests passed against real PostgreSQL with race detection. They use real local append stores and, for the remote gate, two mutually authenticated replica pairs. The [test implementation](../../internal/receiptstore/shards_linux_test.go) exercises the same constructor, acceptance, read, abort, and reclamation interfaces used by the coordinator:

| Gate | Executed evidence |
| --- | --- |
| Canonical catalog | Configuration and pair ordering preserve the binding; changed counts/pairs and reused members fail closed |
| Independent progress and replay | Holding pair A's local gate does not block pair B acceptance, read or abort; holding both selected gates does not block accepted replay or rejection of a changed organization/envelope |
| Same-inbox distribution and retry | Multiple attempts for one inbox use both shards; an expired attempt rejects another pair and retains exactly two registered targets |
| Unidentified history | Removing the catalog from existing authority cannot cause automatic adoption on reopen |
| Read/rewrite ordering | A rewrite forced between target-ID discovery and gate acquisition preserves the live body through freshly queried locators |
| Persisted placement | A synthetic historical placement deliberately differing from the hash is retried on its stored pair without allocating a new receipt |
| Atomic first binding and row integrity | Conflicting concurrent first binds produce one winner; canonical JSON cannot hide altered shard rows |
| Invalid catalog admission | Duplicate members fail validation even while a reused member's gate is exclusively held |
| Authenticated remote locality | With one remote pair unavailable, accepted replay avoids contacting it; another pair still accepts, reads, and confirms abort on its exact registered targets |

The focused run also passed existing legacy replay/lost-commit, remote receipt/rewrite, local hot rewrite/eviction, and preparing-target reclamation regressions. The final focused command was:

```sh
FRY_TEST_DATABASE_URL='<disposable PostgreSQL URL>' \
  go test -race ./internal/receiptstore \
  -run 'Test(Shard|RemoteReceiptAcceptance|RemoteMaintenanceRefreshes|CopiesWithoutReceiptRecover|LostCommitReply|HotRewrite|HotEviction|PreparingTargetsBlock)' \
  -count=1 -timeout=90s
```

The combined `./scripts/tests/receipts.sh -race -short` suite and `./scripts/smoke.sh replicas` subsequently passed with these changes and persisted header metadata. The process smoke still uses one remote pair; the two-pair authenticated case is an integration test. These runs do not prove all of the [design proposal's failure and measurement gates](../design/storage-scaling-next-step.md). The multi-pair tests do not yet independently repeat every killed-writer, owner-epoch replacement, ambiguous rewrite-publication crash, blocked fsync, exhausted-capacity, or long object-store-outage profile. Existing one-pair tests cover several underlying protocols; that is supporting evidence, not proof of every multi-pair failure combination. No real cloud endpoint, independent disks/AZs, production failure domain, representative retained-data restart, or sustained workload capacity was exercised by these focused tests.

The [load harness](local-load.md) accepts an optional fifth argument selecting 1, 2, or 4 local replica pairs:

```bash
./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-one-pair.json 1
./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-two-pairs.json 2
./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-four-pairs.json 4
```

Run them sequentially. These configurations split the same aggregate hot-store budget across pairs, and the report includes accepted attempts per shard. All roots still share one workstation filesystem. Differences in observed throughput cannot be attributed to independent disks or cloud capacity. Mailbox UID allocation, selected-session snapshots, cold header searches and whole-file reclamation remain separate scaling limits.

## Local pair comparison: 2026-10-17

Three sequential fresh-fixture runs each accepted 1,000 messages of 64 KiB using 16 SMTP workers, verified all headers and eight full/partial body samples, and had zero deferrals or failures. All runs used the persisted header metadata implementation. Aggregate member capacity was 675,282,944 bytes in every run.

| Metric | One pair | Two pairs | Four pairs |
| --- | ---: | ---: | ---: |
| Accepted/s | 156.9 | 142.9 | 149.0 |
| Acceptance p50 / p95 / p99 (ms) | 96.8 / 141.0 / 159.8 | 97.7 / 152.0 / 476.8 | 97.6 / 168.1 / 197.4 |
| Last acceptance to all visible (ms) | 158.6 | 249.4 | 272.1 |
| Cold header SEARCH (ms) | 4940.4 | 4966.8 | 5102.6 |
| Repeated header SEARCH (ms) | 58.0 | 38.4 | 43.1 |
| Accepted per shard | 1000 | 507 / 493 | 216 / 278 / 249 / 257 |
| FRY peak RSS (KiB) | 35256 | 33984 | 34932 |

These runs show distribution and correct mailbox results, **not a throughput increase**: 143–157 accepted messages/s varied across the shared-workstation runs. The separate gate tests establish isolation; this same-device comparison does not establish a scaling curve. Report artifacts were `/tmp/fry-load-1000-persisted-header.json`, `/tmp/fry-load-two-pairs.json`, and `/tmp/fry-load-four-pairs.json`. Hardware, measurement definitions, and first-search costs are described in [the load evidence](local-load.md). Independent storage, sustained retained-data workloads, and database profiling are still needed before choosing production topology.
