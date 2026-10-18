# Go dependency remediation — 2026-10-18

The application and test-tool dependency upgrades are applied. Source rescans
report no vulnerable symbols or imported vulnerable packages. Fresh OSV queries
of all six selected module graphs leave one advisory family:
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), concerning the unmaintained
`golang.org/x/crypto/openpgp` packages. None of those packages is imported by the
application, test tooling or either mail fork, including tests. This finding is
retained explicitly; no scanner exclusions, baselines or advisory ignores were
introduced.

This report follows [the initial audit](security-audit-2026-10-18.md) and
`.artifacts/security-audit/dependencies/REPORT.md`. It covers Go manifests,
selected graphs, source analysis and the tests described below. Application
images, native development-tool binaries and test-image OS packages have
separate owners and are not cleared by this source remediation.

## Applied versions

| Scope | Dependency | Before | After |
| --- | --- | --- | --- |
| Root | `golang.org/x/net` | v0.59.0 | v0.61.0 |
| Root | `golang.org/x/text` | v0.42.0 | v0.43.0 |
| Root | `golang.org/x/sys` | v0.48.0 | v0.49.0 |
| Root | `golang.org/x/sync` | v0.23.0 | v0.24.0 |
| Test tooling | `golang.org/x/crypto` | v0.54.0 | v0.58.0 |
| Test tooling | `golang.org/x/text` | v0.40.0 | v0.43.0 |
| Test tooling | `golang.org/x/sys` | v0.47.0 | v0.49.0 |
| Test tooling | `github.com/klauspost/compress` | v1.18.6 | v1.18.7 |
| Test tooling | `github.com/moby/go-archive` | v0.2.0 | v0.3.3 |
| Test tooling | `github.com/moby/sys/user` | v0.4.0 | v0.4.1 |
| Test tooling | `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.69.0 | v0.72.0 |
| Test tooling | OTel API/metric/trace and selected graph SDK | v1.44.0 | v1.47.0 |

The root `x/net` upgrade covers the v0.60.0 fixes for
[GO-2026-6603](https://pkg.go.dev/vuln/GO-2026-6603),
[GO-2026-6610](https://pkg.go.dev/vuln/GO-2026-6610),
[GO-2026-6611](https://pkg.go.dev/vuln/GO-2026-6611),
[GO-2026-6612](https://pkg.go.dev/vuln/GO-2026-6612) and
[GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617).
Test tooling also selects fixed `x/net` v0.60.0 through `x/crypto`.

The tooling changes cover
[GO-2026-6303](https://pkg.go.dev/vuln/GO-2026-6303),
[GO-2026-6354](https://pkg.go.dev/vuln/GO-2026-6354),
[GO-2026-6355](https://pkg.go.dev/vuln/GO-2026-6355),
[GO-2026-6629](https://pkg.go.dev/vuln/GO-2026-6629),
[GO-2026-5841](https://pkg.go.dev/vuln/GO-2026-5841),
[GO-2026-6253](https://pkg.go.dev/vuln/GO-2026-6253) and
[GHSA-mjcv-p78q-w5fw](https://github.com/moby/sys/security/advisories/GHSA-mjcv-p78q-w5fw).
The archive advisory has imprecise symbol information; upgrading removes the
version match without asserting that FRY previously exercised its extractor.

Published versions and module metadata were checked against the Go proxy.
The October `x/*` versions and OTel HTTP instrumentation v0.72.0 were current at
lookup. Compress uses the first fixed v1.18.x release rather than taking the
larger upgrade to the current v1.20.3. Checksums were refreshed with Go tooling.
Tooling tidy also removes unused direct graph declarations for go-spew,
go-difflib and the old YAML module, and selects testify v1.12.1 and its current
YAML dependency.

## Unused graphs and fork manifests

Tidy removes unused `require` pins, allowing vulnerable versions in upstream
module metadata to return. Explicit replacements therefore retain patched
effective versions without adding artificial source imports:

| Manifest | Replacement |
| --- | --- |
| Root | `github.com/yuin/goldmark v1.4.13` → v1.8.6 |
| `tooling/testdeps/go.mod` | `google.golang.org/grpc v1.67.0` → v1.83.2 |
| `third_party/go-imap/go.mod` | `golang.org/x/text` → v0.43.0 |
| `third_party/go-imap/go.mod` | `golang.org/x/net` → v0.61.0 |
| `third_party/go-imap/go.mod` | `github.com/yuin/goldmark v1.4.13` → v1.8.6 |

The root goldmark dependency comes from unused upstream tools metadata. The
tooling gRPC dependency comes from `containerd/errdefs/pkg`; the replacement
matches the root application's existing gRPC version. Updating OTel HTTP
instrumentation raises its unused SDK graph from v1.44.0 to v1.47.0.

The IMAP fork's old go-message/tools graph selected obsolete crypto, net, text,
sys, mod and goldmark versions. Its replacements raise the effective graph;
`x/crypto` now selects v0.58.0. The IMAP `go` directive changes from 1.18 to
1.26.0 because the selected October `x/*` modules require Go 1.26. FRY continues
to use its existing Go 1.27.2 toolchain. The larger IMAP checksum inventory is
module metadata required by the new graph, not additional imported packages.

The fork module names remain `github.com/emersion/go-imap/v2` and
`github.com/emersion/go-smtp`. Their upstream source versions, provenance,
licenses and existing protocol patches remain unchanged. SMTP's manifests are
unchanged. These IMAP manifest changes are local FRY graph hardening, not an
upstream release. Replacement directives in a dependency's manifest are not
inherited by a consuming root; root and standalone fork graphs were checked
separately. Historical `/go.mod` checksum records can remain after tidy and
are not evidence that their old versions are selected or compiled.

Dependency manifests were frozen at **2026-10-18 17:23:39 UTC**, before the
parent changed FRY's root/tooling module paths and source imports to
`github.com/cotocisternas/fry`. The parent owns those canonical names; this
implementation did not rename the upstream fork modules. All scans and tests
below used the canonical tree.

## Vulnerability validation

Govulncheck **v1.9.0**, Go **1.27.2**, Linux/amd64, default build tags,
`GOWORK=off`, `GOTOOLCHAIN=local` and `GOFLAGS=-mod=readonly` were used. The Go
database served modification time **2026-10-08T22:31:09Z**. Tests were loaded by
the scanner, not executed by it. JSON findings were inspected independently
of command exit codes.

The exact source command was the audit's cached `bin/govulncheck` with
`-db=https://vuln.go.dev -format=json -scan=symbol [-test] ./...`, executed in
each module directory using the pinned compiler on PATH.

| Scope | Source scan | Result |
| --- | --- | --- |
| Root | With and without tests | No findings |
| Test tooling | With and without tests | GO-2026-5932, module-only; no package or symbol trace |
| IMAP fork | With tests | No findings |
| SMTP fork | With tests | No findings |

A root scan with tests was repeated after protobuf regeneration and again
returned no findings. Independent fresh OSV queries covered **178 distinct
public module/effective-version pairs**, including all six module roots,
stdlib v1.27.2 and the documented upstream mail-fork versions. Replacements
were queried by the version actually used. Root, test tooling and standalone
IMAP each retain only the OpenPGP advisory on the crypto module; SMTP and both
RPC module graphs return no advisories. This reduces the initial audit's 64
alias-deduplicated graph advisory families to one. The source-free RPC modules
are graph checks, not successful application reachability scans.

`go list -deps -test -json ./...` inventories contain no
`golang.org/x/crypto/openpgp` package in any of the four source module roots.
The root and IMAP fork do not import any crypto-module package. Test tooling
imports patched SSH and its crypto helpers through Testcontainers. The
OpenPGP advisory has no fixed version; upgrading crypto does not remove it.
Removing the entire module would remove required SSH support or alter upstream
dependency metadata. Its unused packages remain an explicit graph finding.

## Tests and remaining validation

All commands below used pinned Go 1.27.2 with readonly module loading:

| Command | Result |
| --- | --- |
| `scripts/toolchain.sh test-go test -json -race -short -count=1 -timeout=180s ./internal/imapsource ./internal/smtpserver ./internal/imapserver ./internal/admintransport` | Pass after generation: 79 passed test/subtest events, 113 skipped; details below |
| `go -C third_party/go-imap test -json -race -count=1 -timeout=180s ./...` | Pass: 77 passed test/subtest events; upstream `imapclient.TestACL` skipped |
| `go -C third_party/go-smtp test -json -race -count=1 -timeout=180s ./...` | Pass: 79 passed test/subtest events |
| `go -C tooling/testdeps test -json -run=^$ ./...` | Pass; compilation only, no lifecycle tests executed |
| `go mod verify` in root, testdeps and both forks | Pass in all four modules |
| `go mod tidy -diff` in root, testdeps and both forks | Pass, empty diff in all four modules |

The focused root results are imapsource **28 pass / 0 skip**, smtpserver
**36 pass / 58 skip**, imapserver **10 pass / 38 skip** and admintransport
**5 pass / 17 skip**. These are Go JSON events, including subtests and parent
tests whose database-backed subtests skip. IMAP source tests exercised real
pipe/TCP verified FETCH completion and UID validation; root preread unit tests
also ran. The complete fork suites exercised malformed APPEND and SMTP
DATA/BDAT framing. Database-backed root RPC, SMTP and IMAP cases were skipped
because no PostgreSQL fixture was supplied. **The full
root real-PostgreSQL `-race -short` run, including HTTP/2 RPC cases, and testdeps
lifecycle tests remain with the parent/image owners after the new test images
are ready.** This report does not represent those skipped cases as passing.

The initial focused invocation failed before tests in smtpserver, imapserver
and admintransport: protobuf `filedesc.(*File).unmarshalSeed` panicked while
loading the generated member descriptor. The parent had changed serialized
generated descriptor strings during module canonicalization without
regenerating their encoded lengths. Normal pinned protobuf generation of both
trees corrected the descriptor; the same focused command then passed. This
was a corrected generation step, not evidence of a dependency regression.
The implementation agent made no production-source edits for it.

## Evidence and scope limits

Raw results and exact commands are in
`.artifacts/security-audit/go-remediation/`: `source-scan-summary.json`,
`graph-osv-results.json`, `openpgp-import-proof.json`, per-module `go/` streams,
`tests/` JSON and status records, and `metadata/` verify/tidy results. The
post-generation root scan is `go/root/symbol-tests-regenerated.*`; the passing
focused rerun is `tests/focused-root-race-short-regenerated.*`. The initial
failed run remains beside it. `sources/` retains primary advisories/version
metadata; `before/` and `manifests-frozen.json` retain the pre-change manifests
and handoff checksums. These local artifacts are excluded from Git.

No setup script, image build, Git commit or publication was performed by this
implementation. Image/native-tool findings from the initial audit require
their own remediation and rescans. These Go results are point-in-time static
advisory checks, not an exploitability proof or coverage of alternate platforms
and build tags.
