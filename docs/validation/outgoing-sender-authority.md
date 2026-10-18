# Outgoing sender authority

Implemented and checked locally on 2026-10-17. Public-mode SMTP submission now requires a live organization-scoped sender grant in addition to authentication and the account's static sender/domain restrictions. The executable wires receiptstore authority into every nonloopback submission listener. Existing plaintext loopback fixtures retain their static bootstrap behavior. This is envelope sender authorization; it is not production readiness or complete message-identity policy.

## Proof and acceptance contract

A scope is either an ASCII domain or an exact ASCII email address. Domains are canonicalized to lowercase; exact-address local parts remain case sensitive. Wildcards, display names, surrounding whitespace, and SMTPUTF8 addresses are not accepted. A domain grant permits any matching envelope sender local part. Exact and domain grants are additive: revoking an exact-address grant does not remove an independently active domain grant.

An assigned organization manager or global operator can create a challenge. The response contains an absolute `_fry-verify.<domain>.` DNS name and a random `fry-sender-v1=...` TXT value. Publish the value as one TXT record at that name. The challenge expires after 24 hours; storage retains its hash, not its raw token. Creating a new challenge supersedes older outstanding challenges for that scope. Revocation and operator override also invalidate outstanding challenges. Creating a challenge does not by itself enable a new grant or revoke an existing active grant.

Verification observes the configured resolver's exact TXT record value. DNS string fragments within one record are already concatenated by `LookupTXT`; separate TXT records are not joined, trimmed, or compared without regard to case. This demonstrates control of the domain's verification DNS, including any delegation. For an exact-address scope it does **not** prove that a particular human controls that mailbox. It is not SPF, DKIM, DMARC, or proof that every From/Sender header is authorized.

The service authorizes the actor and snapshots a current challenge before DNS lookup, releases its database transaction, performs a lookup with a five-second context bound, then opens a fresh transaction and authorizes again. Unknown organizations and unassigned actors do not trigger lookup. Expiry, supersession, revocation, and permission changes during lookup prevent activation. Lookup failure or mismatched TXT never grants authority. Grant/evidence mutation and administrator audit commit atomically. Global-only override requires a recorded nonempty reason; revocation records an explicit reason or a bounded default. [Service and HTTP](../../internal/admin/senders.go), [storage authority](../../internal/receiptstore/sender_authority_linux.go).

A successful grant is a point-in-time observation, valid until revoked. There is no periodic DNS revalidation or automatic expiration of enabled grants. Revocation fences new acceptance: SMTP checks authority at MAIL and before streaming acceptance, while the receipt transaction validates the selected grant/version under a lock at final publication. A race therefore cannot publish a new receipt using a revoked or stale proof. Already accepted mail, idempotent accepted retries, and its queued delivery are unaffected by later revocation. [SMTP checks](../../internal/smtpserver/server.go), [storage tests](../../internal/receiptstore/sender_authority_linux_test.go).

## Administration

Select the organization and open **Outgoing senders** in the administration interface. Create a domain/address challenge, copy its DNS name/value, publish the TXT record, and choose verification. The value is returned on creation, so copy it before leaving the view. Current grants show status/evidence/reason and allow revocation. Global operators additionally see the reasoned override form. Changing organizations hides the prior challenge; responses arriving after the organization/view changes cannot populate the new organization's view.

The CLI authenticates with `FRY_ADMIN_PASSWORD` and the configured administration origin. Flags precede the command:

```sh
go run ./cmd/fry-admin -origin https://admin.example.test:8443 \
  -organization ORGANIZATION_ID -scope example.test sender-challenge

# Publish the returned TXT value, then use the returned challenge ID.
go run ./cmd/fry-admin -origin https://admin.example.test:8443 \
  -organization ORGANIZATION_ID -challenge CHALLENGE_ID sender-verify

go run ./cmd/fry-admin -origin https://admin.example.test:8443 \
  -organization ORGANIZATION_ID sender-grants

go run ./cmd/fry-admin -origin https://admin.example.test:8443 \
  -organization ORGANIZATION_ID -scope example.test \
  -reason 'Domain no longer approved' sender-revoke

# Requires a global operator, not merely an organization manager.
go run ./cmd/fry-admin -origin https://admin.example.test:8443 \
  -organization ORGANIZATION_ID -scope sender@example.test \
  -reason 'Approved exception after external review' sender-override
```

The example identifiers/domains must be replaced. There is no incoming-domain adoption: incoming routing, outgoing authority, and each account's static restrictions are separate policies. [CLI implementation](../../cmd/fry-admin/main.go).

HTTP routes use the existing session cookie, exact configured Origin, and `X-FRY-CSRF` for POST:

| Route | Input |
| --- | --- |
| `GET /api/sender-grants?organization=...` | Organization query |
| `POST /api/sender-challenge` | `organization`, `scope` |
| `POST /api/sender-verify` | `organization`, `challenge` |
| `POST /api/sender-revoke` | `organization`, `scope`, optional `reason` |
| `POST /api/sender-override` | `organization`, `scope`, required `reason` |

For an already authenticated cookie jar and CSRF token, a challenge request is:

```sh
curl --cookie admin.cookies \
  --header 'Origin: https://admin.example.test:8443' \
  --header "X-FRY-CSRF: $FRY_CSRF" \
  --header 'Content-Type: application/json' \
  --data '{"organization":"ORGANIZATION_ID","scope":"example.test"}' \
  https://admin.example.test:8443/api/sender-challenge
```

## Evidence and limits

The PostgreSQL-backed administration package passed its complete race suite with the new service/HTTP routes. Focused storage/administration tests use fake DNS to cover hashing, exact TXT matching, organization isolation, expiry, supersession, replay, revocation, override reasons, audit rollback, permission changes during lookup, and reauthorization. SMTP tests cover live grants and revocation during DATA. These are deterministic protocol/database tests; no live DNS publication/verification was exercised.

A native browser check on a fresh disposable demo at port 8082 passed challenge creation/display, global override, revocation, and delayed-response organization isolation. The CLI end-to-end check passed challenge creation, recorded override, revocation, and scoped listing. The complete PostgreSQL-backed repository race suite subsequently passed with sender authority included. The separate-process smoke also passed HTTPS approval, TLS submission and selected-relay handoff, then revocation between MAIL and DATA with rejection in the same authenticated session. [Admin tests](../../internal/admin/senders_test.go), [SMTP tests](../../internal/smtpserver/sender_authority_test.go).

Remaining policy work includes From/Sender header authorization and the desired null-envelope-sender submission policy. There is no periodic reverification, live DNS validation evidence, or dynamic administrator-managed SMTP submission credential lifecycle; current SMTP accounts and static restrictions are still configured at startup. Public DNS, real certificate/relay accounts, production abuse controls, representative throughput, and HA deployment remain unverified. [Transport limits](public-mail-transport.md), [relay behavior](outbound-relay.md).
