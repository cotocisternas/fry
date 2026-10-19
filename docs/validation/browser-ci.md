# Required Chromium gate

The required browser command runs both existing regressions against a fresh,
seeded native FRY executable and a disposable PostgreSQL fixture. It fails when
either suite, startup, browser preflight or checked cleanup fails. It accepts no
suite-selection arguments and never skips missing dependencies.

From the repository root after native tool setup and an explicit pull of the
[pinned upstream PostgreSQL/Ryuk images](upstream-test-images.md):

```sh
# Explicit download boundary. Ubuntu 24.04 amd64 CI also needs system libraries:
make test-deps-pull
./scripts/browser/setup.sh --with-deps
# On a prepared developer machine, omit --with-deps.
./scripts/browser/test.sh
```

The parent quality adapter can invoke `scripts/browser/test.sh` directly for
`make test-browser`; its setup adapter can invoke `scripts/browser/setup.sh`
with the optional `--with-deps` flag. Browser setup does not rerun native setup,
build Docker images, start services or change the root toolchain fingerprint.
The ordinary test command does not install tools, pull images or download
modules (`GOPROXY=off`, `GOSUMDB=off`). Docker is used only through the FRY-owned
PostgreSQL/Ryuk Testcontainers fixture runner. FRY, Node and Chromium run natively.

## Dependency ownership

[`package.json`](../../scripts/browser/package.json) pins Playwright **1.64.1**;
[`package-lock.json`](../../scripts/browser/package-lock.json) locks Playwright
and playwright-core to that version with npm integrity hashes. The locked
playwright-core browser manifest pins **Chromium 156.0.8078.4, revision 1248**
for Linux amd64. Setup uses that local package's CLI, never an unpinned `npx`
resolution. `npm ci --ignore-scripts` installs the locked package graph, then
`playwright install --only-shell chromium` installs its matching headless build
and FFmpeg dependency. `--with-deps` adds Playwright's Linux system-library
installation, following the [upstream browser installation
instructions](https://playwright.dev/docs/browsers#install-system-dependencies).
CI uses Ubuntu 24.04 amd64 and passwordless sudo for that explicit setup step.

Packages live under `scripts/browser/node_modules/`; browser downloads live under
`.artifacts/browser/browsers/`. Both paths are ignored local dependency output,
not test reports. Setup records hashes of the browser package/lock inputs under
`node_modules/.fry-inputs.sha256`. The gate rejects missing or stale setup and
launches the pinned browser before starting the database. It overrides external
browser/module/origin settings, disables inspector and protocol-debug logging,
and does not fall back to `/usr/bin/chromium`. Direct manual suite execution
still supports `PLAYWRIGHT_MODULE`, `CHROMIUM_PATH` and `FRY_DEMO_ORIGIN` for an
explicitly managed disposable demo.

## Demo lifecycle and output

The browser runner reuses
[`demo_admin.installation`](../../scripts/dev/demo_admin.py), including the
existing example configuration, synthetic SMTP mail and administrator seed.
The interactive `make demo` command uses the same context manager. Browser runs
suppress its credential announcement. Each run chooses random loopback admin
and SMTP ports; an unavoidable short reservation-to-bind handoff fails startup
on collision rather than using a fixed external installation.

The demo's Go build has a 180-second deadline; PostgreSQL startup has a
180-second deadline; listener readiness has a 15-second deadline. Each seed
network operation has a 10-second timeout. Chromium preflight has 30 seconds;
each suite has 200 seconds. Shared process helpers terminate and reap owned
process groups after success, failure, timeout and SIGINT/SIGTERM. Demo shutdown
allows 10 seconds before forced termination and a final two-second reap;
PostgreSQL teardown uses the existing fixture runner's bounded cleanup and
container-absence check. Cleanup failure fails the gate. Ryuk remains the
existing runner's backup for process loss and can briefly outlive its owner
during the normal reaper grace period.

The gate prints the browser versions, suite names, fixed pass/fail statuses and
checked-cleanup success. Raw browser stdout/stderr and server/fixture exception
details are withheld because they can contain credentials, cookies or response
bodies. Demo data, server logs and configuration live only in an owned
`.fry-admin-demo.*` temporary directory and are deleted at teardown. There are
no screenshots, traces, emails, HTML reports or persistent log artifacts to
upload. CI may retain the sanitized command summary.

## Scope and recorded evidence

`admin.cjs` exercises real organization/inbox mutations, stale organization
responses, mailbox credential creation/disablement, sender challenge and audited
override/revocation, response isolation, logout and absence of page errors.
`member-storage.cjs` uses real operator and manager sessions to verify allowed
and denied API access and renders a real local member response. Its existing
intercepted responses exercise exact large decimal counters, unknown runtime
states, bounded pagination, retry and late responses after navigation or session
changes. These remain rendering/ordering fixtures; this gate does not establish
actual runtime storage failures, production OIDC/TLS compatibility, other browser
engines, cloud behavior or recovery guarantees. Neither suite's assertions were
relaxed for integration.

Validated locally on **2026-10-18**, Linux amd64, using the then-present local
dependency images and native pinned Chromium/Playwright above. The later
[hosted CI run](ci-recovery.md) passed this browser gate with the upstream
image pins.

```text
scripts/browser/setup.sh                    PASS
scripts/browser/test.sh                     PASS: admin.cjs and member-storage.cjs
python3 -m unittest scripts.browser.lifecycle_test
                                            PASS: 3 focused checks
python3 -m unittest scripts.tests.scenarios.Configuration scripts.tests.scenarios.ProcessLifetime
                                            PASS: 11 existing checks
```

Python checks were invoked through `scripts/toolchain.sh exec`. The focused
browser lifecycle checks deliberately fail a child against a real demo, confirm
nonzero/sanitized failure reporting, then interrupt an owner with real Chromium
descendants and PostgreSQL running. They verify the owned server, descendants,
PostgreSQL container, scratch data and logs are removed, and cancellation status
143 is preserved. Existing checks cover invalid configuration, build/startup
failure, readiness timeout, resistant descendants and checked cleanup failure.
Shell syntax and `git diff --check` also passed. Local browser setup used the
Ubuntu 24.04 binary fallback on Arch/Omarchy with existing host libraries;
the hosted Ubuntu `--with-deps` execution is separate evidence from this local
run. No dependency images were rebuilt for this validation.
