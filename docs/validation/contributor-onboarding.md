# Contributor onboarding validation

Validated on 2026-10-18 against committed baseline
`0a26c8ea32d42a2fdbf25f2c8854d13c7d3b18ae`, on Linux amd64.
That commit was subsequently reworded to follow Conventional Commits; its
replacement is `a55d73a12a4c83c4b3e405f9871c8dd9faadaf04`, with an identical tree.
The commands below passed in an independent local clone. The in-flight CI,
browser, security and compatibility changes were not present in that commit;
this record does not establish their integrated or hosted result.

## Fresh checkout and prerequisites

The checkout was created with:

```sh
git clone --no-hardlinks /home/coto/dev/fry /tmp/fry-contributor-onboarding.rpGgQU20/checkout
git -C /tmp/fry-contributor-onboarding.rpGgQU20/checkout rev-parse HEAD
```

It initially had a clean Git status and no `.artifacts` directory. No generated
artifacts, native-tool binaries, npm directories or uncommitted source edits
were copied from the working checkout. In particular, the user's in-flight
`internal/smtpserver/server_test.go` edit was absent.

The host already supplied mise 2026.10.12, Python 3.15.0, GCC 16.2.1 and Docker
CLI/daemon 29.9.1, alongside Bash, Make, OpenSSL and the documented host utilities.
Mise's existing Go/Node/Buf installations and host Go/build/download caches were
reused. Setup created this clone's native artifacts itself, including fresh
source-built linter/sqlc outputs and locked npm dependencies. This is a fresh
checkout test with warm host caches, not a clean-machine or offline installation.

The same local Docker daemon already contained the exact current recipe images:

- `fry-testdeps-postgres:8d7dfdf808ddd5f1cbd3d537-amd64`
- `fry-testdeps-ryuk:8d7dfdf808ddd5f1cbd3d537-amd64`

The pinned Debian base was also present locally as
`sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f`,
Linux amd64. The clone verified image provenance/platform with
`make test-deps-check`; it did not repeat the expensive image builds or pull
prebuilt PostgreSQL/Ryuk images. Existing image-build evidence is recorded in
[image remediation](image-remediation-2026-10-18.md).

## Executed checks

Run from the independent checkout:

```sh
make setup-tools
make doctor
make build
make generate-check
make test-deps-check
make smoke SCENARIO=smtp
FRY_DEMO_HTTP_PORT=18083 make demo
```

All terminating Make checks above exited zero. `make setup-tools` verified module
graphs, source-built native tools, installed locked npm dependencies and ran its
doctor. Selected tools were Go 1.27.2, Node 26.12.0, Buf 1.74.0,
golangci-lint 2.15.0 built with Go 1.27.2, sqlc 1.31.1, esbuild 0.28.2 and
TypeScript 7.0.2. `make generate-check` reproduced both Go/TypeScript RPC trees
and the administration browser bundle without changing tracked source.

The SMTP rehearsal reported successful incoming SMTP, authenticated submission,
queued relay, exact IMAP message bytes and process restart behavior.
The demo printed its localhost URL and synthetic fixture credentials. A real
HTTP probe received 200 for `/` with 9,605 HTML bytes and 200 for
`POST /api/login`, with a session CSRF value in the response. No browser UI
assertions were made in this baseline run.

Ctrl-C stopped the interactive demo; Make reported interruption status 130.
After teardown, port 18083 was closed, no `.fry-admin-demo.*` scratch directory
remained, and the clone's tracked and untracked Git status was clean.

Local command logs are retained under
`/tmp/fry-contributor-onboarding.rpGgQU20/`: `setup-tools.log`, `doctor.log`,
`build.log`, `generate-check.log`, `test-deps-check.log`, `smtp-smoke.log`,
`demo.log` and `after-demo.log`. These temporary files are local evidence and
are not committed or durable CI artifacts.

## Contributor CI and remaining limits

The final onboarding instructions are in the [development guide](../development.md#setup).
They now pull digest-pinned upstream PostgreSQL/Ryuk images. The image-build
policy used by this earlier validation was superseded by the user’s clarification;
the historical commands and results above are retained as executed. The contributor gate adds
`make security-setup`, `make browser-setup` and `make check`; hosted Ubuntu 24.04
uses `make browser-setup BROWSER_SETUP_FLAGS=--with-deps` to install its Linux
libraries explicitly. The integrated gate and GitHub Actions execution need
their own result against the final committed source. A workflow definition or
this earlier clone run does not establish that result.

The administration compatibility guard deliberately uses an immutable initial
development-source baseline from `tooling/rpc/admin-baseline.txt`, with an
additional PR-base comparison when supplied. It is not a published API release
baseline. The contributor suite uses short root race/coverage tests; native
arm64 execution, release/recovery rehearsals, AWS/IAM behavior, production
capacity and multi-zone durability remain separate evidence.

The repository is private. The parent verified on 2026-10-18 that GitHub's
branch-protection API returned HTTP 403: the current plan requires GitHub Pro
or a public repository for this feature. Checks can run, but cannot currently
be enforced as required merge checks. The repository remains private on the
current plan; no branch-protection success is claimed.
