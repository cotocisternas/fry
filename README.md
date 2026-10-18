# FRY — The delivery boy

FRY is a Go email gateway for business document exchange. It preserves
conventional SMTP/IMAP workflows while developing receipt storage that combines
replicated hot bytes, PostgreSQL metadata and immutable object-store packs.

The canonical Go module is `github.com/cotocisternas/fry`.

The Linux prototype implements local mail receipt, shared mailboxes, outgoing
queues, external IMAP collection, history import and scoped administration.
Storage members can run locally or as separate processes over mutual TLS.
**Use synthetic data only: FRY is not ready for production.** The schema and
on-disk formats are experimental.

## Start locally

Use Linux amd64 or arm64 with mise, Bash, Make, coreutils and access to a Docker
daemon. The demo also needs Python; process rehearsals may need OpenSSL.
See [development setup](docs/development.md#setup) for build prerequisites.

From the checkout:

```sh
make setup
make doctor
make build
make demo
```

`make setup` installs the pinned native tools and builds FRY's PostgreSQL and
Testcontainers cleanup-helper images locally from repository recipes. Explicit
setup/build steps may download pinned sources and base images. Integration runs
require those local images and fail if they are missing. Go builds, lint and
generation run on the host; Docker supplies disposable dependencies.

The demo starts a localhost installation with synthetic mail and prints its URL
and fixture credentials. Ctrl-C removes its processes and data. For a manually
configured SMTP/IMAP process, see the
[local mail instructions](docs/implementation-status.md#run-a-local-mail-process)
and [example configurations](examples/).

## Find your way around

- [Code map](docs/code-map.md): entry points, module responsibilities, generated
  files and the path from SMTP receipt to mailbox access.
- [Contributing](CONTRIBUTING.md): design principles, behavioral tests and how to
  select checks for a change.
- [Development tooling](docs/development.md): native setup, Make commands,
  integration fixtures and generation.
- [Script guide](scripts/README.md): command launchers, smoke scenarios and
  script directory responsibilities.
- [Architecture brief](docs/design/architecture-brief.md),
  [ADRs](docs/adr/) and [glossary](GLOSSARY.md): product direction, decisions and
  domain language.
- [Storage protocol](docs/design/storage-protocol.md),
  [administration RPC reference](docs/reference/admin-rpc.md) and
  [member RPC reference](docs/reference/member-rpc.md): durable acceptance,
  authorization and transport contracts. Design documents also describe future
  work; the references and validation records identify what is implemented.

## Make a change

Start with the owning module and its contract, then add or update tests for the
behavior you change. Use `make help` to discover commands and the
[contribution guide](CONTRIBUTING.md#choose-checks-for-the-change) to select them.
`make test` is a lightweight run in which PostgreSQL cases may skip; database
changes need the disposable integration runner. A clean lint result or a coverage
percentage does not establish correctness.

Daily work uses a few Make commands selected for the change. Benchmarks and
failure rehearsals remain opt-in because they check different contracts; there
is no requirement to run every script for each change.

## Current limits and evidence

Metadata HA, automatic crash fencing/recovery, replica repair, complete IMAP
compatibility and production-scale scheduling remain unimplemented or unvalidated.
Two directories on one machine do not provide independent failure domains, and
shared NFS storage is unsupported. Actual AWS/IAM behavior, AZ-loss recovery,
representative retained-data capacity and the 30-minute migration cutover target
still need validation. No conversion of older prototype databases is supplied.

The broader browser, security, compatibility and release gates remain unfinished;
the corresponding Make targets report their gaps explicitly. See
[development tooling](docs/development.md#generation-and-tool-upgrades) for details.

[Implementation status and evidence](docs/implementation-status.md) groups the
local validation records, experiments, rehearsals and their limits. These records
preserve the implementation history previously collected in this README.

## License

FRY is licensed under the [MIT License](LICENSE). Third-party dependencies and
the patched copies under `third_party/` retain their original licenses and
copyright notices.
