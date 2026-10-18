# Outbound submission and relay evidence

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

The prototype accepts authenticated outbound submissions, stores original bytes and per-recipient queue state durably, and hands queued messages to a configured SMTP relay. It supports a default relay and an explicit organization override. It never looks up recipient MX records or falls back from an organization override to the default relay.

## Authority and outcomes

Acceptance publishes the receipt, recipient jobs, and live delivery references in the same PostgreSQL transaction. The selected relay ID and retry duration are captured in the immutable acceptance request. The retry clock starts at durable receipt. Configuration must supply an explicit duration; no production default has been selected.

Each recipient is an independent queue job with a deadline, next-attempt time, lease, and monotonically increasing epoch. Terminal handoff/failure and release of that recipient's storage reference commit together. A stale worker cannot update a newer attempt. One recipient's failure does not discard another recipient's bytes or progress. The prototype currently uses one SMTP transaction per recipient; grouping recipients and parallel/fair scheduling are future throughput work.

A successful final DATA reply means **accepted by the selected relay**, not delivered to the final recipient. Lost replies are retried and can deliver duplicates. Tests explicitly demonstrate a relay receiving a message twice after its first final reply was lost. This ambiguity cannot be removed by a local database transaction.

Connection, TLS, greeting, and relay-authentication problems defer delivery. Temporary message rejection retries with bounded exponential delay. Permanent message rejection records failure immediately; an expired retry period also records failure. These are explicit prototype retry choices, pending production policy review. The relay's diagnostic text is not logged or copied verbatim into notices; stored outcomes use bounded generic descriptions.

## Failure notices

A terminal failure atomically records durable notice intent for a non-empty submitting reverse path. A worker creates a multipart/report failure notice through the same outbound acceptance path. Its rendering settings and attempt identity are stable across restart/retry. Notice bytes are stored in the receipt byte store, not copied into a database text field. A null reverse path suppresses further notices if the failure notice itself cannot be handed off.

A notice failing to reach the same selected relay remains an observable queue failure; it cannot guarantee that an unreachable sender receives the notification. Administrator alerts and the administrative UI are still outstanding.

## Submission and transport

Submission is separate from incoming SMTP. Explicit transport modes now support STARTTLS-required submission on 587 or implicit TLS on 465, with one submission listener per process; the omitted/default mode retains numeric-loopback plaintext fixtures. The executable uses persistent salted PBKDF2 credentials with live generation checks. Each account belongs to one organization and requires explicitly configured authorized envelope sender addresses or domains. Unauthenticated submission and another organization's sender are rejected. See [public transport validation](public-mail-transport.md).

Every nonloopback submission listener additionally requires live organization-scoped sender authority. The executable supplies receiptstore grants established by DNS controller proof or a reasoned global-operator override. Static account lists remain an additional restriction. New acceptance is fenced against revocation; already accepted queued mail remains unaffected. Loopback fixtures may retain static bootstrap authorization. Existing configured SMTP usernames support [administrator-managed rotation and disablement](submission-credentials.md), fenced through durable acceptance. New username/sender-scope provisioning and Header From/Sender authorization remain unfinished. See [outgoing sender authority](outgoing-sender-authority.md) for proof semantics, administration and evidence limits.

Relay transport supports verified implicit TLS or mandatory STARTTLS, plus authenticated SMTP. Optional CA bundles support explicitly trusted private certificate authorities. An unauthenticated plaintext relay is allowed only at a numeric loopback IP for synthetic local tests.

Relay IDs now bind permanently to canonical address, transport, and username in storage. Passwords and trusted roots can rotate without changing that destination binding; changing an endpoint, transport or username requires a new relay ID. Existing unbound queued history cannot silently adopt a new destination. Workers must retain the complete catalog needed by pending deliveries and failure notices. Startup rejects omitted destinations required by queued deliveries or pending failure notices. Removing the entire outbound configuration is also rejected while those obligations or currently leased preparing submissions remain. Quiescent installations may disable outbound service. Concurrent workers still require consistently complete catalogs; these checks do not establish distributed configuration rollout or automatic namespace adoption. [Binding implementation](../../internal/receiptstore/relay_binding_linux.go).

## Executed validation

The PostgreSQL/race suite covers independent recipients, durable receipt replay, retry delay, stale-worker rejection, deadline failure, null-sender loop prevention, stable notice enqueue after an ambiguous final mark, authenticated submission, sender restrictions, and default/organization relay selection. Real TCP relay tests cover successful handoff, permanent recipient rejection, lost final replies, and actual delivery of a generated failure notice. Local certificate tests exercise authenticated implicit TLS and STARTTLS, including deferred authentication failure.

The full IMAP suite separately streams a 200 MiB original through the mailbox download path. No high-volume outbound benchmark, real external relay account, AWS endpoint, network partition, or AZ failure was exercised.

[local-mail.json](../../examples/local-mail.json) demonstrates SMTP reception, IMAP, and authenticated submission relayed back into the same local process. It uses `FRY_LOCAL_IMAP_PASSWORD` and `FRY_LOCAL_SMTP_PASSWORD`; its one-hour retry settings are synthetic-example values, not recommended production defaults. `./scripts/smoke.sh smtp` builds and runs the executable with disposable PostgreSQL and synthetic passwords, exercises incoming mail and queued submission through IMAP, and restarts against the same storage.


The complete PostgreSQL-backed repository race suite passed with transport, relay binding, and sender authority included. A separate-process test also verified HTTPS sender approval, authenticated TLS submission, relay handoff, and rejection after revocation between MAIL and DATA.
