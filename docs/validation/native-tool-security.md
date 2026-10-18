# Native tool security remediation — 2026-10-18

Linux amd64 validation after the [original audit](security-audit-2026-10-18.md).
No Docker compiler images, system installation, global Go installation or Git
commit was used. Existing historical binaries and raw audit findings remain
unchanged; normal commands now select repository-local rebuilt CLIs.

## Build ownership and provenance

`mise.toml`/`mise.lock` select Go **1.27.2**, Node **26.12.0** and Buf **1.74.0**
for Linux amd64/arm64. Separate canonical modules under `tooling/native/` pin
golangci-lint **v2.15.0**, sqlc **v1.31.1**, and their complete build graphs.
The linter's selected dependency versions match upstream. sqlc selects
`x/net v0.60.0`, `x/text v0.42.0`, `grpc v1.83.2` and `cel-go v0.30.0`.
CEL's current upstream fixed threshold is newer than the old scanner database;
see the [primary-source research](../research/native-tool-security-builds.md).

`scripts/setup.sh` alone installs/prepares tools. It copies manifests to staging,
authenticates downloads with committed sums and the public checksum database,
verifies the source cache, builds using the pinned compiler, and rejects changed
manifest/checksum files. Source downloads stay in the repository artifact cache.
Builds disable CGO, VCS embedding, experiments and FIPS substitution; use baseline
amd64/arm64 targets and `-trimpath`; and give the linter stable `fry-source` /
`source-build` labels. No current-time or checkout-path value enters the binary.

Each CLI's `*.build.json` receipt records its SHA-256, embedded Go build metadata
and reviewed source/recipe fingerprints. Routine dispatch verifies all of these,
including every embedded module's selected version and checksum, before running
the executable. Missing, stale or altered tools fail with a setup instruction;
ordinary commands do not repair/install them. These local receipts detect stale
or damaged caches; they are not signed third-party release attestations.

The setup fingerprint now covers all prepared root/fork/RPC/testdeps manifests,
the new native tool manifests and build helpers. It was refreshed after the
parent's canonical module rename and dependency updates. Later manifest edits
require setup again.

## Targeted results

- Two setup passes and doctor passed. Rebuilding both source CLIs produced
  byte-identical executables despite different staging directories.
- Seven provenance unit tests passed, covering old compilers/releases,
  dependency/checksum/replacement changes, platform/CGO mismatches, altered
  binary/recipe bytes, and forbidden linter dependency upgrades. Seven additional
  real dispatch checks accepted a valid copied build and rejected missing
  receipts, stale inputs, altered bytes, the old official binary, missing CLIs
  and a stale setup fingerprint. Cached CLI versions also ran with Go proxy and
  checksum-server access disabled.
- Bash syntax and ShellCheck **0.11.0** passed for the affected shell scripts.
  SQL drift/vet and `tooling/verify-native.sh` passed with Docker unavailable,
  including both protobuf trees, browser assets, fixture generation/drift/output
  ownership and refusal checks. The parent owns final generated outputs.
- Trivy **0.69.3**, using the October 18 audit database and no ignore file,
  detected both rebuilt CLIs and both Go generators: **zero findings** across
  all four actual binary targets. Govulncheck **v1.9.0** binary scans also
  reported **zero findings** for both rebuilt CLIs.

Rebuilt executable SHA-256 values:

```text
golangci-lint 4c2fd0512a422cfe9248c513938b732ed7eab23f389e5bd4208727fd4b3453df
sqlc         47abbc2e5e4dc764ea99851724569e93523b2c984078a2eb52813ff8e85b7950
```

Local evidence: `.artifacts/security-audit/native-remediation/` contains the raw
Trivy report/log, both govulncheck streams/logs, Buf function-table/hash evidence,
negative dispatch results, doctor and native verification logs. Setup logs and
build receipts live under `.artifacts/native-toolchain/`. No broad security gate
or full application suite was run in this slice.

## Buf and pending Node release

Buf's **GO-2026-5932** is scoped to deprecated `x/crypto/openpgp` subpackages.
Exact v1.74.0 source dependency graphs for both Linux architectures omit them;
the inspected official amd64 executable and both bundled plugins contain no
OpenPGP functions in their decoded Go function tables. `tooling/native/check-buf.sh`
repeats binary inspection against exact installed artifacts, rejecting missing
or empty function tables. This check is part of `tooling/verify-native.sh`.
The raw module-only match and govulncheck's contradictory wildcard result remain
visible; the [research note](../research/native-tool-security-builds.md) binds the
absence disposition to exact artifact hashes. No general `x/crypto` exemption
or scanner suppression was added. Arm64 source inspection is not arm64 binary
execution evidence.

Node's announced **October 21** high-impact security update remains **pending**:
as of October 18 no fixed version is published. The existing verified pin stays
at 26.12.0; inspect the official announcement/release index after publication and
verify a replacement then. Native arm64 builds/execution, Node native transitive
libraries, host OS security and production suitability remain outside this
targeted validation.
