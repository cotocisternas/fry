# Disposable local kind profile

Run from the repository root only after `fry:local-test` passes
`scripts/smoke.sh containers`:

```sh
scripts/smoke.sh kubernetes fry:local-test
```

**This command creates and deletes a new local kind cluster.** It does not use
an existing kubeconfig/context or create AWS resources. The current profile
requires Docker with `docker save --platform` support, Linux amd64, kind 0.33.0,
kubectl 1.37, Python 3 and OpenSSL. It never builds or pushes an image.

The manifest builders live in `scripts/smoke/kubernetes.py`, rather than a static
manifest containing fixture secrets or reusable ownership IDs. They generate:

- A unique namespace and read-only fixture Secret with a new private CA.
- A digest-pinned PostgreSQL 18 Pod and PVC with official-image initialization.
- One fresh-schema Job (`backoffLimit: 0`), run only before members start.
- Two distinct mTLS member Pods with separate `standard` PVCs and stable IDs.
- A coordinator Pod exposing TLS/STARTTLS SMTP2525 and implicit TLS IMAP1993.
- Internal ClusterIP TCP Services; host access uses numeric loopback forwards.

FRY Pods use `restartPolicy: Never`, UID/GID 65532, read-only roots, no added
capabilities, no privilege escalation, no service-account token and 60-second
termination grace. A member stores its private data child at
`/var/lib/fry/data`, below the local-path claim root. PVC requests do not enforce
local-path capacity; each member's application capacity is bounded to 64 MiB.
The workload is two small synthetic messages, not a load benchmark.

The runner verifies exact originals, unauthenticated member TLS rejection,
cleanly deletes coordinator then members, checks owner closure in PostgreSQL,
recreates member Pods on the same claims, and verifies old and new mail. It does
not initialize schema again during reopen, clear ownership fences, or test
unsafe takeover. Kubernetes restart alone cannot recover an unclosed owner.

All kubectl calls receive the generated absolute kubeconfig and exact context.
Cleanup addresses only the generated cluster name and its own forwards. Logs
and fixture keys are temporary and removed after completion. On failure,
redacted pod/PVC/event/log diagnostics are printed; a JSON success record is
emitted only after protocol and reopen checks pass. Image loading is bounded and
uses single-platform Docker archives for Docker 29 OCI image compatibility. The
pinned PostgreSQL source is inspected before assigning a unique temporary local
tag; the archive and Pod use that name because a digest-only archive can lose
its repository name. The alias must preserve the inspected platform manifest identity. Exported
config/platform/layer data is checked against local inspection; the config JSON
hash must match the imported CRI identity. The report distinguishes the source
index, platform manifest and loaded config IDs. Cleanup removes only that generated local tag.

See [research](../../../docs/research/local-kubernetes-rehearsal.md) and
[validation status](../../../docs/validation/local-kubernetes.md). No Ingress,
cloud load balancer, public issuance, production data or availability proof is
part of this profile.
