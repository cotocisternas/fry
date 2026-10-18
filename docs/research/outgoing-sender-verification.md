# Outgoing sender verification

Investigated 2026-10-17 against primary specifications and first-party documentation. This note evaluates FRY's proposed organization-scoped DNS challenge; it does not claim that the custom `fry-sender-v1` scheme implements ACME or any standardized email authentication protocol.

## Proof scope

Publishing a challenge proves control of the queried DNS validation name as observed through FRY's resolver. It does not establish a person's identity, mailbox existence, ability to read a mailbox, or legal ownership of a domain. ACME describes DNS validation as domain control; Amazon SES separately distinguishes domain verification from verifying an email address through a message delivered to that address. An exact-address grant based on DNS therefore means the domain controller authorized that address, not that FRY verified its individual mailbox user. [RFC 8555 §8.4](https://www.rfc-editor.org/rfc/rfc8555.html#section-8.4), [SES identity verification](https://docs.aws.amazon.com/ses/latest/dg/creating-identities.html).

DNS challenge delegation is normal: CNAME redirects queries, and NS can delegate a validation zone. Let's Encrypt explicitly supports both for its DNS challenge. Consequently FRY's trust includes whoever controls a delegated `_fry-verify` name, potentially a different service from the apex DNS administrator. Do not advertise this as proof that the actor controls every DNS record. [RFC 1034 §3.6.2](https://www.rfc-editor.org/rfc/rfc1034.html#section-3.6.2), [Let's Encrypt challenge delegation](https://letsencrypt.org/docs/challenge-types/#dns-01-challenge).

SES allows a verified domain to cover its subdomains, but that is SES policy, not a consequence of TXT syntax. FRY should define exact-domain matching explicitly: a grant for `example.com` should not silently authorize `sub.example.com`, suffix lookalikes, or a different organization. [SES identity scope](https://docs.aws.amazon.com/ses/latest/dg/creating-identities.html).

## TXT lookup and comparison

One TXT resource record can contain several character strings. Go's `Resolver.LookupTXT` already concatenates those strings into one returned element per TXT record. Compare each element separately to the entire expected value, including the version prefix. Do not join separate returned records, split on spaces, strip quotation marks, trim bytes, or accept a matching substring. Quotes displayed by a DNS console are presentation syntax, not necessarily literal TXT content. [RFC 1035 §§3.3.14, 5.1](https://www.rfc-editor.org/rfc/rfc1035.html#section-3.3.14), [Go LookupTXT](https://pkg.go.dev/net#Resolver.LookupTXT).

Use the absolute name `_fry-verify.<normalized-domain>.`. The trailing dot prevents search-list expansion: Go's resolver source tries only the supplied name when rooted, otherwise it can append configured suffixes. Validate the complete prefixed name's length; a valid maximum-length input domain may become too long after the verification label is added. Use a bounded context with `Resolver.LookupTXT`, and treat lookup errors, missing records, and mismatches as unsuccessful verification. [Go resolver nameList implementation](https://go.dev/src/net/dnsclient_unix.go), [Go LookupTXT API](https://pkg.go.dev/net#Resolver.LookupTXT).

FRY currently restricts domains to ASCII. Validate DNS labels rather than treating an arbitrary ASCII string as a domain; reject address literals, URLs, ports, empty labels and wildcard syntax. Domain comparison can ignore ASCII case, but SMTP local-parts may be case-sensitive. Exact-address normalization must follow an explicit FRY policy consistently at creation and sending; do not assume the DNS case rule applies to mailbox local-parts. [RFC 5321 §§2.4, 4.1.2](https://www.rfc-editor.org/rfc/rfc5321.html#section-2.4).

## Challenge and authorization state

ACME requires unpredictable challenge tokens with at least 128 bits of entropy and additionally binds its response to an account key. FRY's proposed 256-bit random token has ample entropy when generated from a cryptographic random source, but its version prefix supplies no entropy or account binding. Tokens must be server-generated and fresh for each challenge. [RFC 8555 §§8.1, 8.4, 11.3](https://www.rfc-editor.org/rfc/rfc8555.html#section-8.1), [Go crypto/rand](https://pkg.go.dev/crypto/rand).

The following are FRY design requirements inferred from that separation of challenge, identity, and authorization, not requirements imposed on FRY by ACME:

- Persist a unique association between the token/challenge ID, organization, normalized domain, and requested grant scope. A token found in public DNS must not be reusable to verify another organization's challenge or broaden an address grant into a domain grant.
- Authorize the actor and organization before issuing a challenge, exposing challenge details, or querying DNS. After the network lookup, recheck the same current challenge, expiry, supersession, revocation, and actor authorization under the database lock before granting permission.
- The proposed 24-hour expiry applies to the pending challenge. It does not imply a 24-hour grant unless that is separately specified. Replacement challenges and revocation must prevent stale in-flight checks from restoring a grant.
- Check current grants at submission and again in the receipt publication transaction. DNS observation alone cannot serialize a concurrent database revoke. A global override should be a distinct privileged operation with a required reason and durable actor/organization/target audit history; never label it DNS-verified.

The precise prefix, 24-hour deadline, supersession rules, grant lifetime, and override mechanism are product choices. SES's legacy domain-verification API likewise has explicit pending/success/failure state and its own 72-hour verification window; that is precedent for a bounded workflow, not a deadline FRY needs to copy. [SES VerifyDomainIdentity](https://docs.aws.amazon.com/ses/latest/APIReference/API_VerifyDomainIdentity.html).

## Resolver security, privacy, and lifecycle limits

`LookupTXT` returns text and errors, without a DNSSEC validation result in its API. Do not claim that successful lookup authenticates DNSSEC. DNSSEC can authenticate signed DNS data, but validating-resolver trust and its channel to the application still matter; unsigned domains remain outside that protection. DNSSEC also supplies no confidentiality. Deployments requiring authenticated DNS answers need an explicit validating-resolver policy and trusted communication path. [Go LookupTXT API](https://pkg.go.dev/net#Resolver.LookupTXT), [RFC 4033 §§3–5, 12](https://www.rfc-editor.org/rfc/rfc4033.html#section-3).

DNS queries reveal the queried name and may reveal relationships through query traffic; published TXT data is observable. Keep challenge values opaque and avoid embedding usernames, organization IDs, or email local-parts. Authorize before lookup, bound request concurrency/time, and do not accept caller-selected resolver endpoints. [RFC 9076 §4](https://www.rfc-editor.org/rfc/rfc9076.html#section-4).

A TXT-only verifier does not need to fetch HTTP URLs or connect to IP addresses returned by the target domain, so ordinary web-fetch DNS rebinding is not part of this design. This is an architectural inference, not a guarantee about the deployment resolver: recursive DNS can follow delegations, use caches, and return split-view data. Keep the resolver an operator-controlled dependency. Caches and DNS propagation mean a successful observation is evidence at a point in time, not proof of continuous control. [RFC 1034 §§3.6.2, 3.7](https://www.rfc-editor.org/rfc/rfc1034.html#section-3.7), [RFC 4033 §8](https://www.rfc-editor.org/rfc/rfc4033.html#section-8).

Under the proposed policy, grants remain valid until revoked even if DNS records disappear or domain ownership changes. State that limitation directly; continuous control would require a separate revalidation/expiry policy. Finally, this grant authorizes the SMTP envelope sender only in the present scope. It must not be described as authorization of message `From`/`Sender` headers, SPF, DKIM, DMARC alignment, or recipient-side deliverability. Those require separate mechanisms and decisions.
