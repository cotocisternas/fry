# Container packaging: bounded first profile

Research date: 2026-10-17. This is a packaging recommendation for an isolated local image smoke test, followed by Kubernetes manifest review. No image was built or loaded, no cluster or AWS resources were created, and no availability or throughput guarantee was established. The [EKS feasibility constraints](eks-deployment-feasibility.md) still apply, including the unvalidated whole-AZ recovery target.

## Build and runtime images

Registry metadata was queried with `docker buildx imagetools inspect` against the following exact tags. These are registry-reported multi-platform index digests, not locally tested FRY images:

| Purpose | Verified image reference |
|---|---|
| Builder | `docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e` |
| Runtime | `gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3` |

The corresponding Linux amd64 manifests were `sha256:2d658507a011f1eadc0114d7a421b312b2e77d9042a24036a26e267cc9231677` and `sha256:2293b36c7c9082bf4115aab724b4d2cddec82c8eba39bf27ac0517e159acf150`. Pin the index and set the intended build platform; record the resolved platform manifest in build evidence. Recheck availability/security updates when actually packaging. The builder is the Docker Official Image whose published [Dockerfile](https://github.com/docker-library/golang/blob/master/1.27/trixie/Dockerfile) identifies its Go release. The runtime is the upstream [Distroless distribution](https://github.com/GoogleContainerTools/distroless).

Current `go.mod` requires Go 1.25.0; the inspected Go 1.27.2 builder meets that minimum and matches the local toolchain. Set `GOTOOLCHAIN=local` so a later dependency/toolchain requirement fails explicitly instead of silently downloading another toolchain. Go's default `auto` setting can select and download newer toolchains; the module `go` directive is a minimum, not an exact build-version pin. [Go toolchain selection](https://go.dev/doc/toolchain).

Build all four Linux commands with `CGO_ENABLED=0`, `-trimpath`, and `-mod=readonly`; copy `go.mod`, `go.sum`, and **both** local replacement modules under `third_party/go-smtp` and `third_party/go-imap` before module/build steps. Include the relevant repository source afterward. Networked module download is still a separate build input; toolchain pinning alone does not make a hermetic build. Verify the resulting executables are static before choosing this runtime. Distroless static supplies CA certificates, timezone data and `/tmp`, but no shell or package manager; dynamically linked binaries need a different runtime. [Upstream base contents](https://github.com/GoogleContainerTools/distroless/blob/main/base/README.md).

Use one immutable image containing `/usr/local/bin/fry`, `fry-member`, `fry-migrate`, and `fry-admin`, with a JSON/exec entrypoint defaulting to `fry`; override the command explicitly for the others. Exec form delivers signals to the executable as PID 1. Shell entrypoints and shell-based probes/hooks will fail in this runtime. [Docker ENTRYPOINT semantics](https://docs.docker.com/reference/dockerfile/#entrypoint).

## Current executable contract

These are repository observations, not claims made by Kubernetes:

| Command | Runtime needs and exposure |
|---|---|
| `fry` | JSON config, PostgreSQL connectivity, configured TLS keys/CA trust, configured local-member directories or authenticated remote members. Optional packing requires writable bounded scratch storage. Only explicitly configured protocol listeners should be exposed. |
| `fry-member` | JSON config, PostgreSQL, mTLS certificate/key/CA plus coordinator pins, one durable writable `data_directory`, private member TCP endpoint. |
| `fry-migrate` | One-shot operator task using config, database and source credentials; no Service. Final migration activation/freeze remains an explicit operator action. |
| `fry-admin` | One-shot authenticated HTTP client; no Service. A writable output mount is required only for explicit downloads with `-out`. |

Sources: [main fry](../../cmd/fry/main.go), [transport configuration](../../cmd/fry/transport.go), [lifecycle scratch](../../cmd/fry/lifecycle.go), [member](../../cmd/fry-member/main.go), [migration](../../cmd/fry-migrate/main.go), [admin](../../cmd/fry-admin/main.go).

Mount configuration and secrets read-only, readable by the selected nonroot UID/GID; do not bake credentials or private keys into image layers. Use numeric UID/GID 65532 explicitly, `runAsNonRoot: true`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, dropped capabilities and `seccompProfile: RuntimeDefault`. Mount bounded writable `/tmp` only where needed, bounded scratch for packing, and durable volumes for member data. Establish ownership at provisioning or with supported `fsGroup: 65532`; `fsGroupChangePolicy: OnRootMismatch` avoids repeated recursive changes where supported. CSI drivers can implement volume-group handling themselves, so test actual mount permissions rather than assume every driver honors identical behavior. [Kubernetes security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/).

A member root contains durable hot files and ownership state, not disposable cache. Keep its configured identity and storage together; never run two owners against one root. Current ownership fencing intentionally refuses an unclosed owner after a crash. A PVC, StatefulSet, restart policy or successful scheduling does not implement owner recovery or prove safe automatic takeover. See [member owner authority](../../internal/receiptstore/member_owner_linux.go) and the EKS research above.

## TCP routing, readiness and termination

A Kubernetes Service can map conventional TCP ports to unprivileged container ports, for example `25 -> 2525`, `587 -> 2587`, `465 -> 2465`, and `993 -> 1993`. These are proposed mappings, not required FRY port numbers. Keep member/admin endpoints private. Configure listeners with numeric nonloopback addresses such as `0.0.0.0:2525`; default `127.0.0.1` binds cannot receive normal Service traffic. A Service's `port`/`targetPort` mapping is TCP routing, not TLS termination. [Kubernetes Services](https://kubernetes.io/docs/concepts/services-networking/service/).

FRY transport mode is explicit: incoming SMTP supports `starttls` or implicit `tls`; submission supports `starttls-required` or `tls`; IMAP supports implicit `tls`. Public plaintext is rejected. Ports do not select encryption, and pod-local plaintext behind a TLS-terminating proxy is not currently an authorized public transport configuration. Provide certificate names matching client-visible names. No STARTTLS IMAP 143 deployment is proposed.

Current commands provide no dedicated readiness endpoint proving database/member availability. For initial smoke, TCP startup/readiness probes can establish that the configured listener accepts connections; they do **not** verify TLS identity, SMTP acceptance, mailbox publication, authentication or replica durability. A single listener probe does not prove all enabled listeners are healthy. Avoid destructive liveness restart loops based on transient database/member failure, particularly with fenced member ownership. Use external protocol checks for the smoke's stronger assertions. [Kubernetes probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/).

Both servers handle SIGTERM. Current fry SMTP shutdown allows five seconds before cancelling connections; member close uses a fifteen-second context. Start with an explicit 60-second pod termination grace period for the isolated test, then measure complete process and storage closure under blocked I/O; this value is a test setting, not a proven upper bound. Kubernetes runs preStop within the grace budget, then signals PID 1 and eventually SIGKILLs remaining processes; terminating endpoints become unready, but that does not prove upstream load-balancer propagation or existing TCP drain. Avoid shell sleep hooks. Interrupted SMTP and long-lived IMAP must be tested, and forced termination can leave the member intentionally unable to reopen. [Pod termination flow](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination-flow).

## Minimum next validation

Parent implementation should build the pinned image, inspect static binaries and nonroot identity, then run a disposable local network with PostgreSQL, two separately mounted members, generated local CA/TLS credentials, and fry. Publish test ports only on host loopback. Verify TLS and authentication, one SMTP acceptance followed by IMAP search/fetch of exact bytes, member mTLS rejection without credentials, and admin/migration help or a harmless authenticated read. Inspect mounted ownership with the actual runtime UID; verify read-only-root execution and bounded scratch failure behavior. Stop fry first, then members, and verify clean reopen of the same durable roots. Separately interrupt an SMTP transaction and an idle IMAP session during termination; record outcomes and exit timing without treating uncertain acceptance as rejection. Keep crash-owner refusal as an expected safety property, not a restart-success assertion.

This establishes local packaging evidence only. Kubernetes scheduling, CSI persistence, NLB port-25 reachability, certificate rotation, rolling replacement, owner recovery, multi-AZ loss and the five-minute recovery objective remain separate validation work.
