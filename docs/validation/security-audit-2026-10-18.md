# License and dependency/security audit — 2026-10-18

This is a point-in-time audit before Git initialization. It is not an application
penetration test or a completed continuous security gate. No dependency updates,
image rebuilds, credential rotations, repository initialization or publication
were performed during the audit.

Subsequent fixes and validation are recorded in
[repository preparation](repository-initialization.md), with links to the Go,
native-tool and test-image remediation reports. The findings below preserve the
original audit snapshot; they do not describe every dependency after remediation.

## License

The root `LICENSE` contains the MIT License, attributed to `cotocisternas and
contributors`. The README identifies it. The Docker build includes the license
in its source context so the existing license-copy stage can distribute it with
the binaries. The upstream licenses and patch records in `third_party/` remain
intact. Static inclusion checks passed; existing images were not rebuilt.

## Secret scan

Gitleaks **8.30.1**, verified against the upstream latest-release endpoint, ran
in directory mode because this workspace has no Git history. It scanned the
working tree, including hidden files and local artifacts, with full redaction,
archive traversal to depth two and decoding to the tool's default depth five.
Only the audit's own output/cache directory was explicitly excluded to avoid
self-scanning. No finding baseline or inline `gitleaks:allow` suppressions were
applied. Default upstream detection rules and their built-in allowances apply;
Symbolic links were not followed. The tool reported approximately **146.23 MB**
scanned.

Four findings were reviewed; none was identified as a non-fixture credential:

| Location | Classification | Evidence |
| --- | --- | --- |
| `third_party/go-imap/imapclient/client_test.go:51` | Public localhost TLS test key | Exact key bytes match pinned upstream commit `7ac47a9cfd9a06ff5f3bdee52d7420137f023a1e`; used by TLS tests. |
| `third_party/go-smtp/client_test.go:911` | Public localhost TLS test key | Exact key bytes match pinned upstream commit `642c374442498d8e36bc67c27391b5700ac66b89`; used by TLS tests. |
| `.artifacts/rpc/identity.txt:40` | False positive | Go module `h1:` checksum in a build-information dependency record. |
| `.artifacts/rpc/identity.txt:55` | False positive | Go module `h1:` checksum in a build-information dependency record. |

The upstream comparisons used the original
[IMAP test source](https://raw.githubusercontent.com/emersion/go-imap/7ac47a9cfd9a06ff5f3bdee52d7420137f023a1e/imapclient/client_test.go)
and [SMTP test source](https://raw.githubusercontent.com/emersion/go-smtp/642c374442498d8e36bc67c27391b5700ac66b89/client_test.go).
No key material is copied into this report. These known-public keys must remain
test-only. No broad path exclusions or permanent secret-scan suppressions were
added to the repository.

Evidence: `.artifacts/security-audit/secrets/` contains the exact scan config,
redacted JSON report, log, exit code and classification checks. Gitleaks exited
1 because it detected the four reviewed items; the raw scan is not represented
as an unqualified zero-finding result. A pattern scan cannot prove the absence
of every kind of secret, and there is no commit history to inspect yet.

## Evidence handling

Machine-readable scan results, commands, versions, timestamps and diagnostic
logs are under `.artifacts/security-audit/`. They are local audit evidence, not
source files intended for the initial commit. The repository still needs its
planned Git ignore policy before staging files. Reports retain findings rather
than hiding them behind a passing aggregate status.

## Current application dependencies

The root module selects `golang.org/x/net v0.59.0`. Govulncheck reports callable
HTTP/2 symbols for five advisories; each lists **v0.60.0** as the `x/net` fix:

| Advisory | Issue |
| --- | --- |
| [GO-2026-6603](https://pkg.go.dev/vuln/GO-2026-6603) | Server memory exhaustion through trailer headers. |
| [GO-2026-6610](https://pkg.go.dev/vuln/GO-2026-6610) | Acceptance of malformed framing-related headers. |
| [GO-2026-6611](https://pkg.go.dev/vuln/GO-2026-6611) | Excessive CPU consumption through repeated window changes. |
| [GO-2026-6612](https://pkg.go.dev/vuln/GO-2026-6612) | Double flow-control refund on server streams. |
| [GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617) | Server crash through an HPACK encoder race. |

These are static call-path results, including paths through gRPC, not exploit
demonstrations. FRY's Go **1.27.2** toolchain contains the corresponding
standard-library fixes; upgrading Go alone does not update the separate
`x/net` module.

The root dependency must be upgraded and the RPC/protocol regression suites
rerun as a separate remediation change. No version was changed in this audit.

## Go modules and npm

Govulncheck **v1.9.0** analyzed Linux/amd64 with the pinned Go 1.27.2 toolchain,
both with and without tests. Tests were loaded for static analysis, not executed.
The live Go database returned modification time `2026-10-08T22:31:09Z`;
requests were made on October 18. Independent OSV lookups covered **201 distinct
public module/version pairs**, including every selected module from all six
`go.mod` roots. Versions of the two local mail-library replacements were also
queried separately using their documented upstream versions and commits.

| Scope | Result |
| --- | --- |
| Root application | Five `x/net` advisories with symbol traces, detailed above. CVE-record scores classify four High and one Critical (GO-2026-6612, CVSS 9.1); scanner databases can assign different severity scores. |
| `tooling/testdeps` | Seven Go advisories: four with symbol traces, one imported-package-only, two module-only. An additional OSV advisory affects `moby/sys/user`; exact reachability is unknown. |
| `tooling/rpc/tools`, `tooling/rpc/fixture` | No version-matched advisories. These modules contain no Go source: application reachability is N/A, not a successful source scan. |
| Patched IMAP and SMTP libraries | No source-scan findings or direct upstream-version/commit advisories. Standalone IMAP's unused module graph still contains affected versions. |
| npm lockfile | Zero advisories across 57 locked records, including development and transitive dependencies. |

Across complete selected Go module graphs, OSV returned **64 distinct advisory
families** after alias deduplication. This larger count includes unused modules;
it is not a count of reachable FRY vulnerabilities. Root, test tooling and fork
graphs select different versions. Do not apply a standalone fork's graph result
to the version selected by the root application without checking module selection.

Test-tooling remediation candidates:

| Dependency | Scanned version | Advisory fix / required triage |
| --- | --- | --- |
| `golang.org/x/crypto` | v0.54.0 | v0.56.0 covers reported SSH fixes; the unmaintained `openpgp` package has a separate no-fix, module-only advisory. |
| `golang.org/x/text` | v0.40.0 | v0.41.0; call paths include SCRAM authentication through pgx. |
| `github.com/moby/go-archive` | v0.2.0 | v0.3.0; advisory lacks precise symbols, so broad traces do not prove use of a vulnerable extractor. |
| `github.com/klauspost/compress` | v1.18.6 | v1.18.7; module-only finding in the analyzed tooling. |
| `github.com/moby/sys/user` | v0.4.0 | v0.4.1; reported by OSV but not govulncheck; exact symbol reachability unknown. |

The full dependency report and inventory are
`.artifacts/security-audit/dependencies/REPORT.md` and
`go/module-advisory-inventory.json` beneath that directory. They retain primary
advisory links, severities, fixes, per-module selection and reachability. Initial
root/RPC module-scan invocations failed because those directories have no Go
files; corrected scans and complete graph lookups are recorded separately.
One supplementary GitHub advisory request returned 404; the official Go, OSV
and CVE records were retrieved. None of these failures was interpreted as clean.

## Images, native tools and deployment pins

Trivy **0.69.3** scanned eight existing local images by immutable ID using a
newly downloaded database dated **2026-10-18 13:11 UTC**. The previously cached
May database was not used. No images were pulled, rebuilt or executed. Native
Go executable directories were scanned separately; empty-target attempts were
replaced with scans that actually identified the binaries.

| Artifact | Raw findings and interpretation |
| --- | --- |
| Current recipe-built PostgreSQL test image | 254 package findings representing 125 unique advisories, including four raw Critical and 16 unique High advisories. These are mostly OS-package findings, not PostgreSQL-core findings. |
| Current recipe-built Ryuk helper | Four Docker-module matches. The advisories concern daemon components; inspected Ryuk uses client/API behavior, so these are not demonstrated Ryuk vulnerabilities. |
| Available `fry:foundation-local` application image | Five unique `x/net` advisories repeated across three binaries. No exact equivalence with today's source is claimed. |
| Three older FRY application images | Two unique `x/text` advisories per image, from embedded v0.29.0. Current root source selects v0.42.0; do not confuse those old artifacts with current selection. |
| Two older recipe-built test images | Repeat the PostgreSQL/Ryuk inventories; not additional distinct advisory sets. |
| Native Go 1.27.2 (`go`, `gofmt`) | No matched advisory in those two detected binaries. |
| Native golangci-lint 2.15.0 | 13 unique embedded-stdlib advisories; official artifact was built with Go 1.27.0. |
| Native sqlc 1.31.1 | 47 unique advisory IDs across 55 package matches, including its Go 1.26.2 compiler/runtime and older embedded libraries. Not all matches establish reachable vulnerable code. |
| Native Buf 1.74.0 and plugins | One module-level `openpgp` advisory; affected subpackage use was not established. |

The four PostgreSQL-image Critical matches received specific applicability
review. Archive::Tar and MiniZip components are absent, and the 32-bit Perl
precondition is absent from this amd64 image. **CVE-2026-13221 remains unresolved**
because the Debian version classification conflicts with upstream Perl's notes.
All four remain visible in raw results; none was silently suppressed. Remaining
runtime-library findings still require individual review. The recipe uses a
bookworm-main snapshot without a bookworm-security repository, so a recent
snapshot date does not guarantee security updates are included.

Source-built **PostgreSQL 18.6** was checked separately because dpkg-based
scanning does not identify its source installation. Source checksum, image
version strings and headers match the pin. All fixes listed on the retrieved
[PostgreSQL 18 security page](https://www.postgresql.org/support/security/18/)
are 18.6 or earlier; this does not clear the OS libraries.

The pinned development tools were already the latest published releases at the
time of inspection. That does not make their embedded dependencies current.
Addressing golangci-lint/sqlc findings requires appropriately patched upstream
artifacts or a deliberately verified build; selecting the same release again
does not fix its embedded Go/runtime libraries.

Two further pin observations:

- The official [Node October security announcement](https://nodejs.org/en/blog/vulnerability/october-2026-security-releases)
  schedules a high-impact 26.x update for October 21. No fixed release version
  was published at audit time; Node 26.12.0 remains a pending-update observation,
  not a claim that a specific undisclosed CVE was reproduced.
- The disposable kind profile pins Kubernetes 1.37.0. The official
  [CVE-2026-2270 report](https://github.com/kubernetes/kubernetes/issues/142097)
  lists 1.37.1 as the fix. This is a deployment-test pin finding; no production
  cluster was inspected or exists in this project.

Detailed applicability evidence, primary sources and counts are in
`.artifacts/security-audit/images/report.md`, `summary.json` and `findings.csv`.
Unscanned scope includes build-stage base inventories, the full kind-node image,
host Docker/OS dependencies, Node's native transitive libraries, and arm64
artifacts. Advisory matching is not a complete source audit or proof of
exploitability. The broad `make security` gate remains unfinished; these are
explicit one-time scans.

## Remediation order

1. Upgrade root `x/net` to at least v0.60.0 and run RPC/mail-protocol tests.
2. Update affected test-tool dependencies, keeping module-only and uncertain
   symbol findings distinct from confirmed call paths.
3. Refresh authenticated test-image OS/security inputs and rebuild/rescan;
   retain the unresolved Perl classification until adjudicated.
4. Resolve development-tool embedded-dependency findings and the kind pin;
   review the Node security release once published.
5. Rebuild application artifacts from the corrected graph and repeat scans.

These are recorded follow-up actions, not changes applied by this audit.
