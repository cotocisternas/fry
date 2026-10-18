# External IMAP collection evidence

`internal/imapcollect` copies configured source folders to configured FRY folders. `cmd/fry` can run those collectors in background loops. Non-loopback connections require implicit TLS with normal certificate verification; plaintext is accepted only for an explicitly configured loopback IP. STARTTLS, OAuth, and provider-specific authentication are not implemented.

## Receipt and source identity

Each source message is identified by connection ID, source folder, UIDVALIDITY, and UID. Accepted identities are queried in bounded database batches; an accepted high UID never advances a checkpoint past an uncollected lower UID. A repeated run skips accepted identities, and an interrupted attempt retains its original destination/flags/date snapshot for retry.

A connection ID is durably bound to its organization and a fingerprint of remote address, username, and transport mode. Reusing that ID for another account is rejected, preventing accidental collisions with its old source UID history. Password rotation does not change this binding. Source-folder renames or changed UIDVALIDITY create a different source identity and may import the same original bytes again.

The collector streams BODY.PEEK[] through FRY's two-member receipt path. The declared literal length, returned UID, and final successful FETCH response must all be verified before the stream produces successful EOF and acceptance can commit. Source flags and internal date become initial mailbox state; receipt time and recovery retention still start at FRY's durable acceptance. The destination folder is captured by immutable ID, so a later rename does not redirect pending publication.

Collection does not mirror later source changes. Standard persistent flags are copied; custom keywords and Recent are not preserved in this prototype. The source envelope sender/recipient cannot be reconstructed from IMAP: collected receipts record source identity metadata, rather than inventing a transport envelope from message headers.

## Cleanup

Source cleanup is disabled by default. When explicitly enabled it requires UIDPLUS, rechecks authoritative accepted/live receipt access, sets Deleted on the specific UID, and uses UID EXPUNGE. It never uses unrestricted EXPUNGE, which could remove another client's deleted messages. Cleanup failure leaves the accepted identity available for retry. A historic accepted marker whose retained bytes have expired cannot authorize cleanup by itself.

## Executed checks and limits

Real TCP source and destination tests with PostgreSQL/local files cover exact bytes, folder mapping, initial flags, duplicate prevention, independent source changes, and selective cleanup that leaves another deleted source message untouched. A synthetic protocol server sends a complete literal followed by a failed final response; FRY creates no accepted receipt. Unit checks cover short literals, failed stream finalization, and bounded UID expansion. These tests are in the normal PostgreSQL runner.

The collector is currently sequential per connection, polls between batches, and limits a source search result to one million UIDs before expansion. It is not the throughput design for a 500,000-message burst. Fair scheduling, efficient incremental discovery without missing gaps, source-specific compatibility, durable retry observability, and migration/cutover orchestration remain outstanding. Source cleanup is tested with local file members; real independent-site durability is a separate deployment requirement.

Publication errors retain pending originals and record a one-minute retry delay per work item. A deleted or unavailable destination does not block publication of other accepted messages in the batch. Operator recovery of permanently unavailable destinations remains administrative work.

## Local example

[local-collector.json](../../examples/local-collector.json) reads the synthetic source from `127.0.0.1:1143` and writes into a target folder named `Collected`. Run it against a **separate fresh PostgreSQL database** from the source process, using its own configured data directory. Both examples obtain the local demonstration account password from `FRY_LOCAL_IMAP_PASSWORD`. The target listens on SMTP port 2626 and IMAP port 1243. No external server, real credentials, or production mail was used for validation.
