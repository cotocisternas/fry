# Authenticated replica transport

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Implemented and tested locally on 2026-10-17. The coordinator supports two local members or two standalone `fry-member` processes. This is a localhost process-failure prototype, not evidence of independent disks, HA metadata, or an availability-zone recovery guarantee. See the [transport research](../research/replica-transport-implementation.md) for the confirmed HTTP completion failure and ownership constraints.

## Framed streams

`internal/replicastream` encodes bounded data frames and an explicit terminal record. Each frame has the `FR` magic, version 1, a type byte, big-endian 32-bit payload length, payload, and CRC32C over its header and payload. Data payloads are 1–65,536 bytes. The terminal payload is the total 64-bit raw-byte count followed by SHA-256. The receiver requires exact underlying EOF after this record. Invalid headers, oversized frames, count/hash/checksum mismatch, missing terminal records, truncation, and trailing bytes fail explicitly.

The writer exposes `Finish` separately: callers may invoke it only after the underlying storage/source operation has completed successfully. The reader keeps one data frame ahead and withholds the final payload chunk until terminal verification, protecting consumers that use `io.CopyN`. Earlier chunks may have reached staging and cannot independently authorize receipt acceptance. The reader retains two 64 KiB buffers; limits apply to the raw payload rather than framing overhead. HTTP request/response size and time limits remain the transport adapter's responsibility.

Unit/race tests cover empty and multi-frame streams, small consumer reads, sticky errors, writer failures, raw-byte limits, every truncation position of a small encoded stream, and an HTTP response that completes without a storage-success terminal record. A five-second fuzz run exercised 417,375 candidate streams without a panic or limit violation. No throughput or production concurrency claim follows from these checks.

## Process ownership

The private `memberOwner` prerequisite records monotonically increasing process epochs separately from hot-file incarnations. It retains an exclusive PostgreSQL session advisory lock and probes that session. Losing it cancels operation contexts and stops new admission. The database generation guard supports subsequent publication/closure transactions.

An available advisory lock never authorizes takeover of an unclosed generation. Clean shutdown cancels first, waits for tracked operations to release their actual handles, closes the local byte member, then records the generation as closed. Caller timeout stops waiting; it does not create closure proof. A later process can acquire a fresh epoch only after recorded clean closure. An abrupt crash, lost closure commit, cloned disk, or forced takeover needs a later trusted fencing/reconciliation mechanism; this prototype does not silently infer safety from elapsed time or a reconnected database.

Real PostgreSQL tests terminate the ownership connection, verify active cancellation and admission fencing, and prove another claimant remains rejected despite advisory-lock release. Another test blocks shutdown behind active work and verifies closure is not recorded early. Clean reopening increments the generation and rejects stale-generation guards. Receipt target registration, write publication, and cancellation confirmation now check the authenticated process generation. Closed or mismatched generations reject new target registration. Taking process ownership excludes the legacy local write/reclamation adapter; opening a member with an unclosed generation fails before filesystem recovery. Clean restart requires reconnecting the coordinator to the new generation; pending attempts are not silently reassigned.

## Member-owned reads

The private member read path verifies a live coordinator read pin, organization, accepted target, file incarnation, and range. It holds the parent pin row while registering a distinct generation-scoped member pin and a never-reused read-operation identity. Only after the actual hot-store read returns does it atomically mark the operation closed and release its pin. Closure-write failure retains the pin as cleanup debt.

Tests block a member reader, close the coordinator's own read handle, and verify the member pin remains. Releasing the real reader removes that pin and records closure. Injecting a database failure after all payload bytes were read returns failure and retains the unconfirmed pin. Existing cleanup already treats any remaining receipt read pin as protection; expiration alone is not erasure permission.

## HTTP transport and coordinator

`Member` is sealed to FRY's local and remote adapters. `NewWithMembers` preserves the existing receipt contract: both copies and the final PostgreSQL receipt transaction must succeed. Registered targets include member, file incarnation, placement label, and process epoch. Distinct labels remain trusted configuration, not measured physical independence.

The endpoint requires TLS 1.3, verified client certificates from an explicit CA, and an allowlist of coordinator SPKI SHA-256 fingerprints. The client verifies the server certificate chain/hostname and an explicitly configured server SPKI fingerprint, then checks its member ID and placement label. TLS expiry is rechecked on requests/responses over reused connections. The coordinator certificate authorizes trusted internal storage operations across organizations; it is not an end-user credential. Certificate rotation and hot revocation are not implemented.

The first transport uses HTTP/1.1, bounded active requests, bounded raw connections before TLS handshakes, header/body limits, and operation deadlines of at most five minutes. Client proxies, redirects, and transparent compression are disabled. Uploads stream through framed pipes; closing or canceling a call interrupts the owned source. A write acknowledgment follows fsync and generation-guarded locator publication. Reads require a member-owned pin and send a terminal frame only after the actual storage read and closure transaction finish. A disconnected coordinator can release its own pin without releasing the member's pin.

Real PostgreSQL/TLS tests cover two-remote-member acceptance and reads, range reads, wrong CA-authorized client keys, wrong server keys/identity/placement, stale generations, truncated uploads, cancellation while the request is blocked, missing read-closure proof, and lost write responses followed by retry of the same attempt. The final payload frame is withheld on missing closure proof. Reads try another member only before emitting bytes. A separate generated 200 MiB stream passed two TLS-member writes plus a verified read in 1.28 seconds locally, with 3,259,000 total allocated Go bytes during that interval. This is one loopback correctness/allocation check with both endpoints in the test process, not a sustained throughput, RSS, separate-host, or AWS benchmark. The separate-process smoke is a different test with smaller messages.

`ReconcileMemberClosures` bounds each batch to at most 1,000 unresolved reads and 1,000 aborted targets (the executable requests 128 every five seconds). It releases read pins and confirms aborted-target cancellation only when the exact generation has a persisted clean-closure record. Preparing attempts and live/unclosed generations remain untouched. Cancellation confirmation never marks bytes erased. Read-operation identities and owner generations are retained; bounded historical metadata retirement is still required.

## Commands and configuration

Initialize only a fresh, dedicated experimental schema with `fry -config coordinator.json -init-only`; this creates receipt and administration tables and exits. There is no retained-data upgrade path. Then start both members with `fry-member -config member.json`, followed by the coordinator without an initialization flag. Each process reads its database DSN from `FRY_DATABASE_URL`. Metadata replicas/durability are deployment responsibilities; this code cannot prove them from the DSN.

A member configuration contains:

```json
{
  "id": "10000000000000000000000000000001",
  "domain": "configured-zone-a",
  "listen": "127.0.0.1:9443",
  "data_directory": "/var/lib/fry/member-a",
  "max_message_bytes": 1048576,
  "capacity_bytes": 1073741824,
  "max_in_flight": 8,
  "max_requests": 16,
  "timeout_seconds": 300,
  "tls": {
    "ca_file": "/run/fry/ca.pem",
    "certificate_file": "/run/fry/member-a.pem",
    "key_file": "/run/fry/member-a.key"
  },
  "coordinator_keys": ["<64-hex-character SHA-256 of coordinator DER SPKI>"]
}
```

For remote mode, the coordinator's usual configuration adds `replica_tls` with its CA/certificate/key paths. Each of its two `members` entries includes `id`, `domain`, HTTPS `url`, and `server_key` (the server's 64-character hex SPKI SHA-256). Its local `data_directory` and `member_capacity_bytes` are unnecessary in remote mode. The coordinator's external SMTP/IMAP listeners remain restricted to loopback in this milestone. Private storage listeners can bind an explicitly configured network address, but require mutual TLS regardless of address. Protect private keys and the shared metadata database separately.

The runnable configuration generator is [replica_processes.py](../../scripts/smoke/replica_processes.py), invoked by:

```sh
./scripts/smoke.sh replicas
```

The smoke creates a private disposable CA and three real executables against disposable PostgreSQL. It sends and fetches exact messages, gracefully restarts all processes, verifies increasing owner generations and old/new mail, and runs the migration CLI through background copy, final synchronization and activation against the same remote members. It then kills one member. The surviving member still serves mail. New receipt receives no SMTP success: an unconfirmed cancellation may close the connection instead of returning 4xx, so the sender must retry. The killed member cannot silently reopen its unclosed generation. All test storage is local to one machine; this does not simulate an AZ outage.

## Next gates

- Define trusted disk/process fencing and reconcile unclean generations before automatic takeover. Current crash behavior deliberately sacrifices availability rather than inventing closure proof.
- Remote maintenance and lifecycle scheduling are now implemented and exercised in the [running lifecycle stage](storage-maintenance.md). Validate sustained-load behavior, segmented rotation, ownership-aware placement changes, and production-scale inventory bounds.
- Recover or explicitly terminate pending attempts across clean process-generation changes; retire historical operation/owner records with safe replay fences.
- Validate certificate rotation, connection saturation, large remote streams, sustained throughput, fair tenant scheduling, and retained-data scale.
- Implement/validate HA metadata, placement/health authority, replica repair and reduced-protection mode. The current implementation always requires two members for acceptance.
- Perform actual AWS/IAM, disk/AZ independence, backup/restore, and failure drills. Five-minute recovery, six-hour buffering, 24-hour removal, and production throughput remain unproven.
