# Script guide

Run commands from the repository root. Make is the usual interface; `make help`
lists targets. See [development tooling](../docs/development.md) for setup and
check selection, and [contributor guidance](../CONTRIBUTING.md) for choosing
verification that fits a change.
The [layout validation record](../docs/validation/scripts-layout.md) records
earlier relocation checks and process rehearsals.
The [module conversion record](../docs/validation/script-modules.md) covers
removing embedded programs and the subsequent workflow checks.

Daily work needs only a few Make commands selected for the change. The opt-in
failure rehearsals and measurements check different contracts: SMTP/IMAP bytes,
restart durability, restore correctness, deployment startup and storage costs.
Keep those assertions while consolidating repeated wrappers into shared
launchers. Running every script is not a requirement for daily changes.

## Root commands

The small root command surface is:

- [setup.sh](setup.sh): install the pinned native tools and prepare locked
  generator dependencies and module caches. `make setup-tools` runs this step;
  `make setup` also pulls digest-pinned upstream test dependency images.
- [doctor.sh](doctor.sh): check the prepared environment with `make doctor`.
- [toolchain.sh](toolchain.sh): select prepared native tools for `go`, `test-go`,
  `lint`, `sqlc`, `buf`, `exec`, `receipts` and `versions`. Use Make for ordinary
  work; use this adapter for a focused native command.
- [quality.sh](quality.sh): implement the root Make quality targets. Use
  `make check` to reproduce the CI validation suite, or `make build`,
  `make fmt-check lint vet`, `make test-integration` and other focused targets
  from `make help` while developing.
- [container.sh](container.sh): compile FRY natively, verify static Linux outputs
  and package them in the pinned distroless runtime. Use `make container
  IMAGE=fry:local-test ARCH=amd64`; no compilation runs inside Docker.
- [test-deps.sh](test-deps.sh): pull, identify, check and run disposable local
  test dependencies. Prefer `make test-deps-pull`, `make test-deps-check`,
  `make test-deps-test` and `make test-deps-lint`; direct `image` and
  `run -- COMMAND ARGS` actions are described in the
  [fixture lifecycle reference](../tooling/testdeps/README.md).
- [browser/setup.sh](browser/setup.sh) and [browser/test.sh](browser/test.sh):
  `make browser-setup` explicitly installs locked Playwright/Chromium;
  `make test-browser` runs both browser suites with a fresh disposable demo.
- [security/setup.sh](security/setup.sh) and [security/check.sh](security/check.sh):
  `make security-setup` source-builds the pinned scanners; `make security` runs
  the contributor security gate.
- [smoke.sh](smoke.sh): dispatch one explicit process or deployment rehearsal.
  Use `make smoke SCENARIO=...`, or the direct interface below.
- [bench.sh](bench.sh): dispatch one opt-in measurement with
  `scripts/bench.sh CASE [args]`.

Setup needs the [host prerequisites](../docs/development.md#setup), including
Linux amd64 or arm64, mise, Bash, Make, Python 3, OpenSSL and a C compiler.
Integration fixtures require Docker access and digest-pinned upstream dependency
images. FRY owns their Testcontainers setup and lifecycle code. Kubernetes has
additional prerequisites in its [profile README](../deploy/kubernetes/local/README.md).

Normal tool selection does not install missing tools. Setup downloads pinned
tools and upstream images. Smoke scenarios require their images to exist
locally; the launcher does not build or pull images. Native process scenarios
build temporary FRY test executables.

## Demo and smoke rehearsals

```bash
make setup
make doctor
make demo
# Run each rehearsal separately:
make smoke SCENARIO=smtp
make smoke SCENARIO=replicas
make smoke SCENARIO=offline-restore

# Explicit application image build before deployment rehearsals:
make container IMAGE=fry:local-test
make smoke SCENARIO=containers IMAGE=fry:local-test
```

The pinned distroless base must be pulled explicitly before `make container`;
see the [packaging prerequisites](../docs/validation/local-containers.md).
The Kubernetes scenario is currently on a
[security hold](../docs/validation/local-kubernetes.md) and exits before creating
resources. It is not an available next step after building the image.

`make demo` directly invokes [dev/demo_admin.py](dev/demo_admin.py) as
`scripts.dev.demo_admin` through the repository toolchain. It starts a disposable
localhost installation with synthetic mail and prints its URL and fixture
credentials; Ctrl-C triggers cleanup.

The direct launcher supports help without starting dependencies:

```sh
make smoke                    # defaults to help when SCENARIO is unset
./scripts/smoke.sh --help
./scripts/smoke.sh smtp
./scripts/smoke.sh offline-restore
./scripts/smoke.sh containers fry:local-test
```

Its interface is `scripts/smoke.sh SCENARIO [IMAGE_TAG]`. Mail scenarios are
`smtp`, `replicas` and `submission-credentials`. Storage scenarios are
`offline-restore`, `cold-restore`, `automatic-rotation`, `member-storage`,
`schema-startup` and `storage-quotas`. Deployment scenarios are `containers` and
`kubernetes`.

Only `containers` and `kubernetes` accept `IMAGE_TAG`, which defaults to
`fry:local-test`; other scenarios accept no positional arguments. The equivalent
Make option is `IMAGE`. When its security hold is lifted, Kubernetes is intended
to create and remove a disposable local kind cluster. The [container validation](../docs/validation/container-runtime.md) and
[Kubernetes profile](../deploy/kubernetes/local/README.md) describe each rehearsal.

## Opt-in measurements

Use `scripts/bench.sh CASE [args]`; each case retains its positional arguments
and defaults. Choose fresh output paths when capturing another run:

```sh
./scripts/bench.sh --help
./scripts/bench.sh acceptance baseline /tmp/fry-acceptance-NEW
./scripts/bench.sh load-mail 1000 65536 16 /tmp/fry-load-NEW.json
./scripts/bench.sh query-plans /tmp/fry-plans-NEW.json
```

Cases are `acceptance`, `hotstore-retained`, `load-mail`, `mailbox-publication`,
`query-plans` and `retained-reclaim`. Their implementations are Python modules
under [bench/](bench/) with underscores in module names. The shared launcher
replaces the six per-case shell wrappers. Measurement definitions, limits and
examples are in the [acceptance](../docs/validation/acceptance-profile.md),
[retained-storage](../docs/validation/retained-storage.md),
[load](../docs/validation/local-load.md),
[publication](../docs/validation/mailbox-publication.md) and
[query-plan](../docs/validation/mailbox-fairness.md) records.

## Implementation directories

- [dev/](dev/): interactive local development, including the administration demo.
- [bench/](bench/): synthetic load, acceptance profiles, retained-storage
  measurements and PostgreSQL query-plan reports. Invoke these explicitly with
  fresh output paths; they are separate from ordinary quality checks.
- [browser/](browser/): focused browser regressions against the disposable demo.
  `make browser-setup` prepares pinned Playwright/Chromium; `make test-browser`
  owns the real demo, both suites and cleanup. The explicit setup option
  `BROWSER_SETUP_FLAGS=--with-deps` also installs Linux libraries on supported
  distributions; CI uses it on Ubuntu 24.04.
- [ci/](ci/): lightweight Git-event classification for the Actions workflow.
  It recognizes a conservative Markdown-only diff before the expensive job;
  it does not replace `make check` or provide an unchecked success path after
  classification/test failure. See [CI selection](../docs/development.md#ci-and-local-verification).
- [generate/](generate/): SQL and protobuf generation adapters. Prefer
  `make generate`, `make generate-check`, `make sqlc` and `make proto-generate`.
- [lib/](lib/): shared shell/Python toolchain and fixture lifecycle mechanics.
  These helpers are imported or sourced by their consumers.
- [ops/](ops/): explicit operational probes, currently the opt-in AWS S3 probe
  described below.
- [security/](security/): explicit scanner source builds and security checks,
  using the pins and policy under `tooling/security`.
- [smoke/](smoke/): scenario implementations. The public SMTP command remains
  `scripts/smoke.sh smtp`, backed by [smtp.py](smoke/smtp.py). The replica adapter
  is [replicas.sh](smoke/replicas.sh).
- [tests/](tests/): receipt integration runner, existing-code baseline and
  tooling regressions. Prefer `make test-integration`, `make baseline-check`,
  `make test-scripts` and `make lint-policy-check` where applicable.

Shell owns tool selection and short command orchestration. Configuration,
report, protocol and process logic belongs in normal Python modules; JavaScript
tooling belongs in ordinary JavaScript files. Follow the
[language ownership standard](../CONTRIBUTING.md#respect-source-ownership-and-explain-the-change):
no Python/JavaScript heredocs or `-c`/`-e` program strings as permanent
implementations. Text and configuration heredocs are fine.

From the repository root, invoke Python modules with `python3 -m` through the
toolchain adapter when they need prepared tools:

```sh
./scripts/toolchain.sh exec python3 -m scripts.smoke.offline_restore
./scripts/toolchain.sh exec python3 -m scripts.tests.processes
./scripts/toolchain.sh exec python3 -m scripts.tests.lint_policy
```

Prefer the smoke/bench launchers and Make targets for their prerequisite checks.
Some internal modules need arguments or fixtures supplied by their owning launcher.
`make test-scripts` runs Docker-free Python unittest discovery under
`scripts/tests`; `make test-script-processes` retains the focused process timeout
and cleanup checks.

## AWS probe and current limits

[ops/validate-aws-s3.sh](ops/validate-aws-s3.sh) is a separate, explicit opt-in
probe. Read the [AWS operator runbook](../deploy/aws/README.md) and inspect
`./scripts/ops/validate-aws-s3.sh --help` before supplying its configuration,
exact bucket acknowledgment and new absolute manifest path. The live probe uses
temporary credentials and a dedicated bucket, performs billable operations,
and leaves markers requiring separate operator cleanup. It does not provision
resources and is not part of setup, smoke or normal quality targets.

`make test` may skip PostgreSQL cases; required integration targets need Docker
and prepared local images. The existing-code baseline uses the short root race
suite and complete fork race suites. `make check` adds the browser, security and
protobuf compatibility checks used by CI. See
[CI and local verification](../docs/development.md#ci-and-local-verification)
for the setup sequence and evidence limits. `make release-check` continues to
report incomplete release requirements.

Rehearsals use synthetic data and local failure domains. Their results do not
establish production readiness, AWS/IAM behavior, independent disks or zones,
automatic crash takeover, or sustained workload capacity. Script paths and
commands in the documentation follow the current layout; historical validation
results and hashes continue to describe their recorded runs. Updating those
references does not supply new runtime evidence.
