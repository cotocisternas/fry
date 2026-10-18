# Native tool security builds

Researched 2026-10-18 against upstream releases, source and advisories. This
supplements the original [audit](../validation/security-audit-2026-10-18.md);
it does not rewrite the findings for the old release binaries.

## Verified source-build inputs

The published [golangci-lint v2.15.0 release](https://github.com/golangci/golangci-lint/releases/tag/v2.15.0)
resolves to commit `25c5369a93aacb8cccea0d46e1569ca4a194564d`;
[sqlc v1.31.1](https://github.com/sqlc-dev/sqlc/releases/tag/v1.31.1) resolves to
`a95e91d70ad9e1181253c333a1cfdd75ae4b95a5`. Both are the latest published
releases checked on this date. Their existing official binaries embed older
compilers/dependencies, as recorded by the audit's native inventory.

Build both with the repository's **Go 1.27.2**. Its October 8 release includes
security fixes in the compiler toolchain's `go` command and several standard
library packages. Set `GOTOOLCHAIN=local` to keep execution on the explicitly
selected compiler. [Go release history](https://go.dev/doc/devel/release#go1.27.2),
[toolchain selection](https://go.dev/doc/toolchain#select).

Preserve golangci-lint's release dependency selections. Upstream warns that
manually changing its dependencies produces an unsupported combination, and
that importing it into an application's tool module can change either graph.
Its release build uses `CGO_ENABLED=0`, `-trimpath` and the
`main.version`, `main.commit`, `main.date` linker variables. A dedicated,
verified build must keep those dependency selections and identify itself as a
repository build, with a deterministic build-date value.
[Installation guidance](https://golangci-lint.run/docs/welcome/install/local/#install-from-sources),
[tagged release recipe](https://github.com/golangci/golangci-lint/blob/v2.15.0/.goreleaser.yml).

sqlc documents source installation, and its
[tagged module](https://github.com/sqlc-dev/sqlc/blob/v1.31.1/go.mod) selects
the older components reported by the audit. Override the following in an
isolated build graph; do not change the application or test-tooling modules.
[sqlc installation](https://docs.sqlc.dev/en/v1.31.1/overview/install.html).

| Component | Minimum build selection | Primary fix evidence |
| --- | --- | --- |
| `golang.org/x/net` | `v0.60.0` | [GO-2026-6611](https://pkg.go.dev/vuln/GO-2026-6611) and the other October HTTP/2 advisories retained in the audit. |
| `golang.org/x/text` | `v0.41.0` | [GO-2026-6629](https://pkg.go.dev/vuln/GO-2026-6629), crafted-input panic in the PRECIS Nickname profile. |
| `google.golang.org/grpc` | `v1.83.2` | [Upstream xDS denial-of-service advisory](https://github.com/grpc/grpc-go/security/advisories/GHSA-2v4p-qf9q-27wj). |
| `github.com/google/cel-go` | **`v0.30.0`** | [Current upstream advisory](https://github.com/cel-expr/cel-go/security/advisories/GHSA-gcjh-h69q-9w9g) lists `v0.22.0`–`v0.29.2` affected and `v0.30.0` fixed. The saved Trivy database's `v0.29.0` threshold is stale. |

Pin complete module manifests/checksums, source identity, compiler, target and
build flags. Go authenticates downloaded modules against `go.sum` and, where
applicable, its checksum database; `go mod verify` additionally detects
post-download cache mutation. Preserve those protections and verify cached
executable bytes/provenance before reuse. `-trimpath` removes local filesystem
paths from compiled output. These are separate controls; a version string
alone does not establish provenance.
[Module authentication](https://go.dev/ref/mod#authenticating),
[cache verification](https://go.dev/ref/mod#go-mod-verify),
[build flags](https://pkg.go.dev/cmd/go#hdr-Compile_packages_and_dependencies).

## Buf OpenPGP applicability

[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) affects the deprecated
`golang.org/x/crypto/openpgp` package and six subpackages, at all versions. It
does **not** cover every package in `x/crypto`. The advisory recommends the
maintained ProtonMail fork; it does not identify that fork as affected.

For [Buf v1.74.0 source](https://github.com/bufbuild/buf/tree/36fdbc523bcbda32e63aaa31aaa147d40e480307),
commit `36fdbc523bcbda32e63aaa31aaa147d40e480307`, the following check succeeded
with the pinned Go compiler, `GOOS=linux`, `CGO_ENABLED=0`, `GOWORK=off` and
`GOFLAGS=-mod=readonly`, separately for `GOARCH=amd64` and `arm64`:

```sh
go list -deps -f '{{.ImportPath}}' \
  ./cmd/buf ./cmd/protoc-gen-buf-breaking ./cmd/protoc-gen-buf-lint
```

The complete graphs contain 884 and 882 packages respectively, with **no
OpenPGP package**. Their only external `x/crypto` packages are `internal/alias`,
`chacha20`, `internal/poly1305` and `chacha20poly1305`. The checked source's
`go.mod` SHA-256 is
`30b7798d67c2d26020d945ad9a95e0540282112be4b563698510761930d9c74f`;
`go.sum` is
`d8753e7e867ffc9e2b59b33af804946879cea715a8e45d41385041aedc12ab73`.

The installed official Linux amd64 executables are stripped, so `go tool nm`
reports no symbols. Reading their `.gopclntab` using Go's
[`debug/elf` and `debug/gosym`](https://pkg.go.dev/debug/gosym) recovered 61,578,
31,191 and 24,384 function records respectively, with **zero OpenPGP function
names** and positive matches for the expected crypto packages.

| Inspected executable | SHA-256 |
| --- | --- |
| `buf` | `92326d957d613712669fb55f9e4340946a8142a92120e09008459118778273d2` |
| `protoc-gen-buf-breaking` | `3c648017ff6128b7e757dcb2a172bcf2253d7fb724a4ebae8210d5ca6f954432` |
| `protoc-gen-buf-lint` | `ba31eb65fc65c303ee90e332423e72d622cdb017fd4f5499cb49e09717230194` |

Disposition: **affected component absent for the inspected Linux amd64
artifacts**, corroborated by both target source graphs. Keep Trivy's module
match visible. A supplementary govulncheck v1.9.0 binary scan reported wildcard
OpenPGP pseudo-symbols for `buf`; these conflict with the decoded function table
and do not establish actual OpenPGP calls. The two bundled plugins produced no
govulncheck findings. Do not present that scanner discrepancy as a clean scan,
or extend this disposition to changed Buf versions/artifacts. Native arm64
binary execution/symbol inspection was not performed.

## Node: pending October 21 release

The [official announcement](https://nodejs.org/en/blog/vulnerability/october-2026-security-releases)
schedules security releases for the 26.x, 24.x and 22.x lines on or shortly after
**2026-10-21**, with highest impact **HIGH**. On **2026-10-18** it supplies no
fixed release version or disclosed CVE details; the
[official release index](https://nodejs.org/dist/index.json) still lists
`v26.12.0` as current. Record the update as **pending**, retain the existing
verified pin, and check the announcement/index after publication before choosing
and verifying a replacement. No future version or completed remediation is
claimed.
