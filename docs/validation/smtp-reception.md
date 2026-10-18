# Local SMTP reception evidence

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The Linux prototype now accepts SMTP over a loopback TCP listener and publishes original bytes through the PostgreSQL/two-local-member receipt contract. `cmd/fry` runs this path from explicit JSON configuration. It is not a public MX server or authenticated submission service.

## Exercised behavior

Real TCP clients and raw protocol transcripts cover address resolution, multiple destinations, alias deduplication, exact raw bytes, connection reuse, recipient rejection, size boundaries, interrupted DATA/BDAT, admission limits, shutdown, and failed storage. A database lock test verifies that SMTP success waits for authoritative receipt publication. Injected ambiguity after a real committed receipt resolves through status; unavailable status closes the connection without falsely declaring definite failure.

`./scripts/tests/receipts.sh -short` and `./scripts/tests/receipts.sh -race -short` passed with these cases. The adapter uses real PostgreSQL and real local files. `./scripts/smoke.sh smtp` separately builds and runs the executable, sends synthetic mail, restarts it against the same storage, and checks persistent receipt counts and deduplicated destinations.

The pinned `go-smtp v0.25.0` source is retained under `third_party/go-smtp` with its MIT license. Two narrowly scoped framing corrections prevent accepting truncated BDAT LAST payloads and allow an exactly-at-limit DATA payload. The patch and provenance are recorded in [FRY-PATCHES.md](../../third_party/go-smtp/FRY-PATCHES.md). Upstream tests, including their race run, pass with the corrections. Run them separately because nested Go modules are excluded from the root suite:

```sh
go -C third_party/go-smtp test -race ./...
```

## Scope limits

Routes are static and ASCII-only. One SMTP transaction belongs to one organization; a recipient resolving to another organization receives a temporary failure so the sender can retry separately. This needs broader transaction handling before public ingress. Public-edge TLS, authorization administration, Received trace headers, and production abuse controls are outstanding. Subsequent stages add [local authenticated submission and queued relays](outbound-relay.md) and [IMAP collection](imap-collection.md). Optional filtering is not enabled.

The two members are local files in one process, not independent sites. Neither these tests nor the executable establish AZ-loss durability, throughput, five-minute recovery, or real AWS behavior. An uncertain outcome may require sender retry, which can yield a separate accepted receipt: SMTP has no universal exactly-once message identity.
