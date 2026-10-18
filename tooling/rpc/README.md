# Native protobuf and browser tooling

Mise selects FRY's native Go, Node and Buf tools. `make setup-tools` prepares the
Go generators from `tools/go.mod` and `tools/go.sum`, and JavaScript tools/runtime
libraries from `package.json` and `package-lock.json`. It is the explicit install
step; generation commands do not install tools or use Docker.
Use Make targets where available; the [script guide](../../scripts/README.md)
covers launchers and the implementation directories.
`make doctor` uses [check-packages.cjs](check-packages.cjs) to compare installed
generator package versions with the lockfile.

```sh
make setup-tools
scripts/generate/proto.sh identity
scripts/generate/proto.sh lint proto/admin
make proto-generate
make generate-check
make native-tools-check
```

The wrapper uses local `protoc-gen-go`, `protoc-gen-connect-go` and
`protoc-gen-es`. No remote Buf plugins, registry schema dependencies, standalone
protoc or Connect-ES generator are needed by these contracts. The same locked npm
installation supplies TypeScript, esbuild and browser runtime dependencies.
Generated code is never edited by hand.

Inputs contain all imported local schemas, with Buf-provided well-known types
supported. The wrapper supplies its own Buf configuration with STANDARD lint,
then compiles descriptors and generates both languages. Go uses
`paths=source_relative`; protobuf-es uses `target=ts,import_extension=js`.

Each dedicated output root contains `go/`, `ts/` and `.fry-rpc-generated`.
Generation stages successfully before replacing a marked output tree. It refuses
nonempty unmarked output, overlapping input/output paths, symlink inputs and
invalid ownership markers. Replacement removes stale generated files. `check`
regenerates into scratch and compares the complete tree without changing it;
missing, changed or extra outputs fail.

Native execution preserves the 64 MiB input, 64 MiB per-file and 128 MiB total
output budgets, 10,000 output-file cap and 180-second generation deadline. It
uses a temporary workspace and cleans it on exit. It does not provide Docker's
network, memory, process or filesystem sandbox. Go fixture dependency resolution
is offline after setup; plugin discovery is local.

`internal/admintransport/build-web.sh check` typechecks and bundles the browser
client in scratch, then compares the committed bundle. Its `generate` action
copies the resulting bundle only after a successful build.

`tooling/rpc/verify.sh` exercises the public wrapper: lint/build, generation,
drift rejection without overwriting, stale-output removal, handwritten-output
refusal, symlink refusal, deterministic fixture regeneration and host ownership.
The synthetic fixture compiles generated Go and TypeScript and bundles a browser
client. `tooling/verify-native.sh` also verifies actual SQL/admin/member/browser
outputs with Docker deliberately unavailable.

These tooling checks do not replace runtime RPC interoperability, authorization,
mTLS, cancellation or streaming tests. They also do not establish compatibility
with a released administration API or native arm64 execution.
