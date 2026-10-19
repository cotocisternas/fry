# Local Kubernetes rehearsal

**Security hold, 2026-10-18:** the runner exits before invoking commands or
creating resources. Kubernetes **1.37.1 or later** is required to fix
[CVE-2026-2270](https://github.com/kubernetes/kubernetes/issues/142097), but the
official kind release and registry currently publish no verified patched node
digest. The former 1.37.0 image is no longer a runnable default. Restore `NODE`
in `scripts/smoke/kubernetes.py` only after verifying an official patched digest
and explicitly provisioning it locally; the rehearsal never pulls images.
See [image remediation](image-remediation-2026-10-18.md) for the recorded
primary-source checks and Docker-free hold regression. The current database
fixture uses the [digest-pinned upstream PostgreSQL image](../../tooling/testdeps/README.md)
with FRY-owned Testcontainers lifecycle code. Its Docker readiness checks and
the [hosted CI result](ci-recovery.md) do not constitute a fresh Kubernetes
rehearsal.

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Executed 2026-10-17. **Passed** with `./scripts/smoke.sh kubernetes fry:local-test`.
Independent source review found no remaining P1/P2 issues. The latest run used
the rebuilt application including persisted SMTP credential operations, mailbox
organization turns, cleanup progress, shared inbox admission and bounded header
capture. Individual mailbox publication remains in use. It completed protocol checks, clean
same-PVC replacement and final owner closure, then deleted its generated cluster
`fry-kind-6518628b55e0` and temporary PostgreSQL image alias. A subsequent
read-only check found no kind clusters, no matching node container and no
matching temporary image alias.

The tested application image identities were:

- Index: `sha256:ed4890c6ed4291e81e9dbf7d64db168955f3ac6c2bd532dc737f47c099f950fb`
- Linux amd64 platform manifest: `sha256:f3e0fe8afff504abf38ad34b5404fdc9b30985e306022fa909dec51f0695fb24`
- Loaded config: `sha256:371ea625019b10e23b8279fe420cd7b970e162d9add36be446a2865f0e2abff2`

Entry point: `scripts/smoke.sh kubernetes [IMAGE_TAG]` (default
`fry:local-test`). The [profile README](../../deploy/kubernetes/local/README.md)
describes prerequisites and generated resources. The
[research](../research/local-kubernetes-rehearsal.md) records upstream node-image,
storage, security and isolation evidence.

The recorded execution used one disposable kind 0.33.0 control plane with pinned
Kubernetes 1.37.0, synthetic PostgreSQL 18, two separately mounted authenticated
remote members and one coordinator. It verified TLS SMTP acceptance and IMAP
exact originals, unauthenticated member rejection, owner closure on graceful
termination, same-PVC clean reopen and old/new message access. It records the
application index/platform/config identities and profile/node image in a JSON
success result (`messages: 2`, `clean_same_pvc_reopen: true`). No
benchmark or automatic crash-restart claim is made.

Both synthetic messages matched their complete original bytes through IMAP,
including the first message after replacement. All three FRY pods ran as UID
65532 with read-only roots and zero unexpected restarts. The three PVCs bound to
distinct volumes; both member owners closed before deletion and reopened at
epoch 2 on the retained claims. The final cleanup also verified owner closure.

Failure must not be repaired by resetting durable owner rows, replacing member
claims or rerunning schema initialization. A failed unclosed member startup is
an authority safeguard. Local claims share host disk, and cluster deletion
removes their disposable data. This rehearsal provides no EKS/NLB integration,
AZ-failure recovery, production mail, throughput or storage-capacity evidence.

## First-run image import failure

The initial run observed `ErrImageNeverPull` for PostgreSQL: exporting its digest-only
Docker reference imported it into kind under an `import-*` repository name, so
the Pod's original PostgreSQL reference was absent with pull policy Never. The
runner now assigns the inspected pinned source a unique temporary local tag,
checks that the alias preserves its Linux amd64 platform manifest, saves/loads the
named alias and uses exactly that reference in the Pod. It verifies CRI image
lookup/config identity before creating Pods. Docker29 local inspection returns
index/manifest IDs while CRI returns a config digest; the runner therefore hashes
the exact exported config JSON, checks its platform/layers/runtime settings
against local inspection, and compares that hash to CRI. The report separates
pinned source, index/platform manifest, alias and loaded config identities. Cleanup removes only the generated tag. Image/config waiting
errors now fail immediately instead of consuming the readiness timeout.

Syntax and offline image checks preceded successful cluster execution; the
latest rebuilt-image run retained the corrected import path.
The corrected import verified PostgreSQL config digest
`sha256:29754c7520f4b9654cf8ff98e19ac73b88e255c3a53f6236ebe9d52249bb6df3`.

The historical node image was
`kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5`;
the historical database source was
`postgres@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336`.
The rebuilt image includes the credential, scheduling and intake changes; their
dedicated evidence remains in [submission credentials](submission-credentials.md),
[mailbox query plans](mailbox-fairness.md), [cleanup](storage-maintenance.md),
[acceptance profiling](acceptance-profile.md) and [header capture](header-capture.md).
This two-message Kubernetes rehearsal does not independently retest every
operation or measure performance under competing traffic.

## Storage maintenance rebuild

Rehearsal session `82258` passed with `fry:storage-20261017` after its container
rehearsal passed. The generated cluster `fry-kind-793040e2554a` was removed by
checked cleanup. Evidence: `/tmp/fry-storage-kubernetes.log`.

- Application index: `sha256:61e4dfcca29ff97464484c8dad49601d5121854f4e70ba783d7343b0bda252d4`.
- Linux amd64 platform manifest: `sha256:272116968f87d2ce09c7d82b577b384e385e2f82b80928a6a2b1175df728ed2b`.
- Loaded config: `sha256:c37271ae2276bd3c2a44b8d4d417280604c696ceebb3f41218b61f6f8f296a5c`.

The two originals, clean same-PVC reopen, authenticated members, distinct claims,
UID 65532, read-only roots and zero unexpected restarts passed again. This image
contains the sealed-file, automatic-rotation and close-proof changes; the fixture
retains uncapped member profiles and does not independently exercise those
maintenance operations. That run reused the historical node/database pins above
and had the same local-only limitations.

## Versioned-schema rebuild

Run `40016` passed with `fry:schema-20261017`; checked cleanup removed generated
cluster `fry-kind-4aa779054616`, its volumes and temporary PostgreSQL image alias.
Evidence: `/tmp/fry-schema-kubernetes.log`. A subsequent `kind get clusters`
reported no remaining clusters; unrelated Docker services were untouched.

| Application artifact | SHA-256 |
| --- | --- |
| Index | `16b7ac3dd0d38c9dd62364fc5919148521a71b09014160e4149bfaff64c48dac` |
| Linux amd64 manifest | `9fc3b583fb4ddc171c802a9acff057782427c805b2d614f31e56859ffd7c2245` |
| Loaded config | `248c8b2e62c00a467ee41e139ad9935c876cbc2b2998919276357d2bea8de239` |

Fresh atomic initialization and subsequent compatibility checks succeeded before
actual TLS mail operations. Two exact originals, clean same-PVC reopening,
unauthenticated member rejection and distinct PVC bindings passed. All three FRY
pods retained UID 65532, read-only roots and zero unexpected restarts. This image
also contains SMTP capacity deferral and member diagnostics, whose dedicated
tests remain their detailed evidence. That run reused the historical node and
PostgreSQL pins above; it was a one-host synthetic rehearsal, not EKS or AZ recovery.
