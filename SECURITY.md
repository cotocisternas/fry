# Security checks

Run `scripts/security/setup.sh` after native tool setup, then
`scripts/security/test.sh` and `scripts/security/check.sh`. `make security-setup`
and `make security` integrate these commands into contributor setup and the
required `make check` CI gate. Installation is explicit; checks never install
or repair tools. The commands support the existing Linux amd64/arm64 native
toolchain and Linux hosted CI. CI needs a complete Git checkout (`fetch-depth: 0`).

The gate includes:

- Govulncheck source analysis with and without tests in the root, both mail
  forks and `tooling/testdeps`. Tests are loaded, not executed. Every finding
  is evaluated, including module-only findings, regardless of scanner exit code.
- Fresh OSV queries for complete selected graphs in those four modules, both
  RPC modules and the security scanner module, using effective replacement
  versions. Local fork versions are also queried at their documented upstream
  versions. These graph matches do not establish source reachability.
- Govulncheck source analysis including tests of all three pinned security
  scanner commands, plus binary analysis of the validated native
  golangci-lint/sqlc executables. Native tool graphs are covered by their built
  binaries rather than querying all unused upstream module metadata.
- `npm audit --package-lock-only` for every Git-source npm lockfile, including
  development and optional dependencies. High and Critical findings fail;
  lower severities remain counted. No npm lifecycle scripts run during auditing.
- Gitleaks against every tracked working-tree file, including hidden files,
  plus untracked files not ignored by Git, and all locally available Git history
  (`--all --full-history`). A secret committed and deleted still fails. Local
  T3 checkpoint refs can produce repeated findings absent from hosted clones;
  these counts are occurrences, not distinct credentials. Historical secrets
  in unavailable remote refs cannot be examined. Ignored untracked caches are
  outside the source snapshot. Tracked caches/submodules fail rather than
  silently escaping coverage. Symlinks are scanned as Git link text; their
  tracked source targets are scanned independently.
- Actionlint against all Git-source GitHub workflow files. Its optional
  ShellCheck and Pyflakes integrations are disabled; shell scripts are checked
  separately. The required lint gate already enables gosec, staticcheck and
  govet. A second equivalent Go static analyzer is not required here.

The scanner tools and their complete build graphs are pinned in
[`tooling/security/scanners/go.mod`](tooling/security/scanners/go.mod) and
`go.sum`: Gitleaks **v8.30.1**, govulncheck **v1.9.0**, and actionlint **v1.7.12**.
They are built with the mise-pinned compiler, authenticated module checksums
and checksum database, fixed build settings, and locally checked build receipts.
Gitleaks' older embedded dependency graph is raised to fixed versions in this
module. Ordinary checks reject altered/stale tools. The receipts detect cache
damage/staleness; they are not signed release attestations. Vulnerability
databases remain live inputs, so identical source may fail when a new advisory
is published. Network, scanner, parsing and incomplete-inventory failures fail
the gate. No offline-success fallback exists.

Scanner commands are analyzed from authenticated sources instead of applying
govulncheck binary mode to Gitleaks: binary mode reports wildcard OpenPGP symbols
despite their absence from its independently loaded package inventory. This is
the same scanner behavior recorded for Buf in
[`docs/validation/native-tool-security.md`](docs/validation/native-tool-security.md).
No wildcard package/symbol exemption was introduced; the source check must pass
the strict package/symbol policy below.

## Reviewed exceptions

[`tooling/security/policy.json`](tooling/security/policy.json) is the entire
repository-specific exception policy. Exceptions have evidence, review dates
and hard expiration dates; expiration requires a new review, not an automatic
extension. Changes to the policy require the same review as other gate changes.

The two public upstream localhost TLS test keys qualify only when the scanner's
exact secret bytes (SHA-256), test-file path and `private-key` rule all match.
These exceptions expire **2027-01-18**. Moving a key, changing its bytes/rule,
adding another key in the same file or copying it into application source fails.
Default Gitleaks rules and their upstream built-in allowances apply. There are
no repository path/rule exclusions, broad baselines, inline allowances or
`.gitleaksignore` suppressions; the gate explicitly bypasses inline allowances
and repository ignore files.

The only Go exception is **GO-2026-5932** for exactly `golang.org/x/crypto
v0.58.0` in the named root/IMAP/test-tool/scanner graphs. It expires
**2026-11-18**. The gate independently loads dependencies including tests and
requires all affected `openpgp` packages to be absent. A package or symbol trace,
new version, new scope, different advisory or missing import inventory fails.
The prior rationale is in
[`docs/validation/go-remediation-2026-10-18.md`](docs/validation/go-remediation-2026-10-18.md).

## Evidence and scope

Only `.artifacts/security/summary.json` is suitable for a CI artifact upload. It
contains status, stage counts, database metadata and fixed diagnostic reasons;
it contains no source snippets, secrets, commit authors or email/test logs.
Do not upload the artifact directory recursively. Local private diagnostics are
under `.artifacts/security/private/`; scanner build receipts/binaries are under
`.artifacts/security-tools/<architecture>/bin/`. Secret values are consumed in
memory solely to compare exact hashes; neither raw secret reports nor secret
stderr are written to disk or printed. Local secret location reports retain
only file/rule/line and acceptance status. Files are created with private modes.
A failed run invalidates any earlier passing summary before scanning begins.

The gate checks the default Linux build configuration on the executing host
architecture. It does not execute application tests, replace the browser or
gosec lint gate, audit host OS packages or container images, scan unavailable
Git refs, or certify release/production deployment security. Those are separate
checks. It does not publish anything or require repository credentials.

For a suspected vulnerability, use a private channel to the repository
maintainer. Do not attach credentials, mail contents or raw scanner output to
public issues or CI artifacts.
