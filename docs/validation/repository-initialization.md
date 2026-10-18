# Local repository preparation — 2026-10-18

The project remains **FRY — The delivery boy**. Its canonical root Go module is
`github.com/cotocisternas/fry`; FRY-owned nested modules use the corresponding
repository directory paths. The two patched upstream mail libraries retain their
upstream module identities and license notices. The root license is MIT.

Go imports, protobuf `go_package` options, generated clients, package-based lint
rules and coverage package keys use the canonical paths. SMTP/IMAP behavior,
binary names, protobuf service namespaces and runtime storage identifiers were
not renamed. An intermediate edit of generated descriptor strings caused an
initialization panic; normal generation from the canonical protobuf definitions
corrected it. Generation drift checks and runtime tests verify the result.

## Repository contents

The local Git repository uses `main`. Source, generated clients, schemas,
documentation, pinned tool manifests, upstream notices and shared agent skills
are included. `.gitignore` excludes local artifacts, caches, credentials/config
overrides and personal editor state. No remote or publication is part of this
preparation.

A source-only export was prepared from Git's index, without copying the working
checkout's artifact cache. Setup, doctor, build, generation checks and all 42
script tests passed from that export. Tool installation remained native; no
compiler containers or prebuilt PostgreSQL/Ryuk images were used.

Gitleaks 8.30.1 scanned the source-only export with full redaction, decoding and
archive traversal. Its two findings are the public upstream localhost TLS keys
already verified in the [original audit](security-audit-2026-10-18.md). There is
no new non-fixture credential finding, and the raw scan is not called a
zero-finding result. Audit caches and raw scanner output remain local artifacts.

## Security and validation evidence

- [Go dependency remediation](go-remediation-2026-10-18.md): patched graphs,
  source scans, fork race suites and the unused OpenPGP advisory.
- [Native tool remediation](native-tool-security.md): reproducible patched
  lint/sqlc builds, provenance checks and binary scans.
- [Test-image remediation](image-remediation-2026-10-18.md): signed security
  snapshots, current immutable image IDs, remaining OS findings and kind hold.

All four freshly built native FRY executables were detected and scanned by
Trivy 0.69.3 with zero matched vulnerabilities. The scan used the audit database
updated on October 18. Previously built application images were not relabeled or
represented as rebuilt artifacts; rebuild them before reuse.

Strict lint, formatting, vet, SQL/protobuf/browser generation checks, native
generation without Docker, and the test-dependency runner's complete suite
passed. The root PostgreSQL-backed race suite passed with `-short`; longer load
and failure rehearsals remain outside this run. An independent review of the
tooling, image changes and canonicalization found no actionable issue.

The initial aggregate coverage check flagged two helper-command packages after
all root tests passed. Added tests cover malformed configuration, missing client
credentials and listener cleanup when readiness output fails. Both helpers pass
their focused race suite. Their new profile was combined with the preserved full
root profile using the reporter's duplicate-block accounting; the aggregate
passes all unchanged package floors. Helper results are 114/139 statements for
member maintenance and 30/43 for the S3 fixture. No production behavior or floor
was changed to satisfy this check.

These results do not clear every test-image OS match, provide arm64 runtime
evidence, complete the planned continuous security/release gates, or establish
production readiness. Node's announced October 21 security release is pending;
the kind rehearsal remains disabled until an official patched image is verified.
The linked reports retain these limits and the remaining advisory details.

Local aggregate evidence is in `.artifacts/pre-git/`, including clean-export
logs, the source secret scan, executable scan, full root race/coverage results,
fixture tests and helper follow-up coverage.
