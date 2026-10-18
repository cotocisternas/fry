# Local container runtime rehearsal

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

This rehearsal runs the actual FRY image with a disposable PostgreSQL 18 container and two authenticated remote storage members. It uses no AWS resources, real mail domains, production credentials, or existing database/volume. Containerization does not establish production durability, HA, workload capacity, or a safe automatic takeover procedure.

## Run

Prepare native tools/dependency caches and the pinned runtime base as described
in [native container packaging](local-containers.md), then build and run:

```sh
make container IMAGE=fry:local-test ARCH=amd64
./scripts/test-deps.sh pull
./scripts/smoke.sh containers fry:local-test
```

Docker, Python 3, and OpenSSL must be installed on the host. Packaging compiles
the four commands natively with pinned Go, then copies prepared outputs into
Distroless. The smoke script consumes the already-built image and explicitly
prepared official PostgreSQL fixture; it never builds the application image or
pulls missing fixture images. All fixture containers, the private internal bridge
and separate coordinator ingress bridge, and named member volumes have generated
names and labels; cleanup uses those exact names, never global Docker pruning.
Interrupts execute bounded cleanup. The signing CA key stays on the host outside
the read-only runtime mount.

The current crash-restart check compares the exact stored owner state before and
after restart refusal. It does not depend on detailed error text from the member
command, which now sanitizes startup errors. The historical checked flow and
image identities below describe their original executions.

The FRY containers use the image's default UID/GID 65532, a read-only root filesystem, all capabilities dropped, and `no-new-privileges`. The image provides `/var/lib/fry` owned by 65532 with mode 0700. Each member mounts a separate newly created named volume at that path; Docker's volume initialization supplies its initial ownership. Configuration, public certificates and **synthetic fixture private keys** are mounted read-only and made readable by the runtime UID. This permissive fixture-key readability is not a prescription for production secret permissions.

The PostgreSQL fixture uses its official image's normal initialization/runtime behavior, rather than FRY's UID and filesystem policy. It publishes no host port and is reachable only on the generated internal bridge network. The coordinator is attached to both bridges before its process starts; the regular ingress bridge contains no database or member containers. This supports host publishing on Docker versions that ignore published ports on internal-only networks. Only the coordinator's SMTP and IMAP ports are published, to random **127.0.0.1** host ports. No Docker socket or host executable is mounted into FRY containers.

## Checked flow

The full rehearsal passed on 2026-10-17 using Docker 29.9.1 on the Linux workstation. The tested local image ID was `sha256:3ee749995be539e20eea3ccf33f71530d8ebd4d25e47012cc5ec25ef163f26b7`. It verified these gates:

1. Run `fry -init-only` once against a fresh schema. This initializes receipt and administration metadata without contacting members; it is not rerun on restart and is not a schema upgrade mechanism.
2. Start `member-a` and `member-b`, each with its own volume, stable member ID and failure-domain label. Their internal DNS names match generated server certificate SANs. The coordinator presents a separate client certificate, and members authorize its SPKI fingerprint in addition to CA validation.
3. Start the coordinator using SMTP `starttls` on container port 2525 and IMAP implicit `tls` on 1993. The host verifies the private test CA and hostname/IP. Send one synthetic message through STARTTLS, wait for mailbox publication, and fetch its exact bytes through authenticated IMAP TLS. Incoming SMTP must not advertise submission AUTH.
4. Stop coordinator first, then members with a 25-second Docker grace period. Require successful exits and closed owner generations. Restart the same containers with retained volumes/configuration, require new owner epochs, reread old bytes, then accept and reread another message.
5. SIGKILL one member. Reread accepted originals through the surviving copy. A new SMTP transfer must produce a temporary failure or close without a success reply; the durable receipt count must remain unchanged. Unconfirmed cancellation intentionally cannot promise a final 4xx response.
6. Restart the exact killed container and volume. First prove the dead process's PostgreSQL advisory lock is released, then require the explicit busy/unclosed-generation diagnostic, no new listening marker, the previous generation still unclosed, and no new owner epoch. An unrelated startup error cannot satisfy this check. The runner never clears fencing flags, substitutes a root, or otherwise manufactures takeover authority. Stop the remaining healthy services and remove all disposable resources.

See [runner implementation](../../scripts/smoke/container_processes.py),
[smoke.sh containers](../../scripts/smoke.sh),
[replica transport evidence](replica-transport.md), and
[public mail transport validation](public-mail-transport.md).

## Filesystem and operational limits

Member roots must retain their complete lock files, active file, staging/generation directories, and storage identity. `replicaconfig.Directory` creates the configured directory and fsyncs it and every ancestor through `/`; readonly-root compatibility therefore depends on the actual runtime/filesystem supporting those directory operations. This rehearsal exercises the selected Docker/filesystem combination rather than assuming all read-only mounts behave identically. Local volumes still share one host and do not prove independent failure domains.

A remote-only coordinator without packing needs no writable local store. Enabling object packing additionally requires durable writable `scratch_directory`; object storage and packing are outside this minimal rehearsal. Normal application logging goes to stderr, not a writable log file. System CA certificates remain necessary for externally verified TLS services; the generated private CA here is only a fixture trust root.

There is no dedicated health endpoint. Readiness checks in this runner observe process/listener startup and then perform real protocol operations; a listening socket alone does not prove successful two-copy acceptance. Unclean member restart is deliberately fenced, so a generic container restart policy is not automatic recovery. Real secret management, certificate renewal, external fencing/reconciliation, DNS, public exposure, production observability, and Kubernetes deployment remain separate work.

## Execution status

Shell/Python syntax checks passed. The first image rehearsal reached member and coordinator startup but stopped before protocol checks because Docker did not publish ports on the internal-only network. The runner now gives only the coordinator a second, regular ingress bridge. A bounded disposable container probe verified loopback host publishing with this dual-bridge topology and removed its resources. The corrected full rehearsal subsequently passed the protocol and lifecycle checks above. That result applies to the recorded image; later source changes are covered by their separately documented checks.

Image inspection confirmed the four Linux/amd64 ELF executables contain no dynamic interpreter, the system CA bundle is present, and `/var/lib/fry` has mode 0700 with UID/GID 65532. Each command also started with `-help` under nonroot/read-only/no-network/capability-drop restrictions. That historical build used digest-pinned official Go and Distroless images, disabled implicit toolchain downloads, copied local module replacements, and preserved dependency notices; see [packaging research](../research/container-deployment.md). Current builds use the [native packaging workflow](local-containers.md). No image was pushed to a registry. The current smoke does not separately test unauthenticated member rejection, interrupted SMTP uploads, active IMAP termination, or persistent object-packing scratch volumes; existing protocol/process tests provide separate evidence for parts of those paths.

## Storage maintenance rebuild

The complete rehearsal passed again with local image `fry:storage-20261017`,
index ID `sha256:61e4dfcca29ff97464484c8dad49601d5121854f4e70ba783d7343b0bda252d4`.
This build includes sealed-file reclamation, conditional automatic rotation and
the current-reclamation/terminal-close proof repair. Build session `50914` and
runtime session `5784` exited successfully; logs are
`/tmp/fry-storage-container-build.log` and `/tmp/fry-storage-container-runtime.log`.
The fixture uses uncapped member profiles, so this run verifies packaging,
protocol operation and ownership lifecycle; the dedicated feature tests remain
the evidence for rotation and reclamation behavior. No registry push occurred.

## Versioned-schema rebuild

Build `26856` and container rehearsal `42261` passed with
`fry:schema-20261017`, index
`sha256:16b7ac3dd0d38c9dd62364fc5919148521a71b09014160e4149bfaff64c48dac`.
Logs: `/tmp/fry-schema-container-build.log` and
`/tmp/fry-schema-container-runtime.log`. This image includes atomic schema
initialization and the compatibility gates in coordinator/member/migration
commands, plus the earlier SMTP capacity-deferral and member-diagnostics changes.
The unchanged container rehearsal again verified TLS mail, nonroot/read-only
operation, retained-volume restart, survivor reads, deferred acceptance with a
member down and refusal of an unclosed owner restart. The feature-specific
validation records establish their detailed behavior; this two-message fixture
adds packaging/startup evidence. No AWS resources or registry push were involved.
