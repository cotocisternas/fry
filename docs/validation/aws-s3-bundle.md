# Real-S3 validation bundle: local preparation

Commands and links use the current layout. Recorded results and hashes describe
the original runs and source files, including any wrappers since removed.

Prepared on 2026-10-18. The opt-in tooling, CloudFormation resource template and
[operator runbook](../../deploy/aws/README.md) are ready for an isolated São Paulo
test. **No AWS credentials were used, no resources were provisioned, and the live
test has not run.** The next prerequisite is an AWS account with an operator
identity using temporary credentials and permission to prepare the dedicated
bucket and roles.

## Local evidence

Object-store race tests passed in `1.037s`, including the new independent local
validation checks and the existing SDK/HTTP-fixture tests. Session `74485` used:

```sh
env -u FRY_AWS_S3_VALIDATION_CONFIG \
  -u FRY_AWS_S3_VALIDATION_BUCKET \
  -u FRY_AWS_S3_VALIDATION_MANIFEST \
  go test -race -count=1 -v ./internal/objectstore
```

The log explicitly records `TestAWSS3Validation` as skipped because it was not
opted in: `/tmp/fry-aws-validation-local.log`. `go vet ./...` also passed
(`/tmp/fry-aws-validation-vet.log`). The combined shell command later exited 1
because the ShellCheck shim had no selected version; that was a tool-selection
failure, not a Go test failure. Running ShellCheck explicitly through
`mise exec shellcheck@0.11.0 -- shellcheck scripts/ops/validate-aws-s3.sh` passed
(`/tmp/fry-aws-validation-shellcheck-final.log`), without changing a global default.
Bash syntax, the wrapper's help path and template JSON parsing passed separately.

Independent tests cover all partial/full opt-in combinations, exact bucket
acknowledgment, unsafe region/endpoint/profile/owner configurations, strict and
bounded JSON loading, FIFO/directory/symlink rejection, manifest
0600 permissions and no-overwrite behavior, complete key serialization without
credentials, temporary assumed-role identity checks, and the distinction between
HTTP 403 `AccessDenied`, other 403 errors and conditional 412 failures. The FIFO
case has no writer during the assertion and confirms rejection before opening.

Implementation and independent review checked manifest-before-write ordering,
credential snapshot binding, joined race operations, expected-owner headers,
explicit denial assertions and the template dependency graph. Only test/helper
code and deployment artifacts changed in this slice. STS was already pinned in
the dependency graph; `go mod tidy` merely marked it direct because the new test
imports it. No module version or `go.sum` entry changed.

## What remains unverified

The live test uses the actual adapter and AWS SDK, but local gate tests cannot
establish IAM enforcement or real S3 behavior. The template has not been validated
by CloudFormation or deployed. The runner observes bucket settings and probes a
bounded set of reads, writes and denied deletions. It does not mutate bucket
configuration to test denial, claim comprehensive policy verification, or run
production load or failure recovery.

Both the wrapper and live test disable SDK configured service-endpoint overrides
before credential loading. The profile files, credential-process executables,
proxies and host trust configuration remain trusted; this is not a universal
credential-provider sandbox. The retirement identity remains a trusted
conditional writer; the template cannot enforce FRY's current marker-body and
user-metadata semantics. See the [primary-source findings](../research/aws-s3-validation-bundle.md).

The five-key intent manifest survives failure. Successful live validation would
leave three marker objects; the provisioning identity must revoke writers and
perform separate verified cleanup. A live test PASS would not mean that cleanup
had completed. No AWS PASS or cleanup outcome is recorded here.

## Prepared artifact identities

| Artifact | SHA-256 |
| --- | --- |
| `internal/objectstore/aws_validation_test.go` | `95c2b12847ceda005bc58467ad8fc8f685aa8d1f97a8fe0b070d6a1495c8e245` |
| `internal/objectstore/aws_validation_local_test.go` | `a5765adfd13993e90bca8a62c45404afd3d68e11a364cbf9181370bd43397987` |
| `deploy/aws/s3-validation.json` | `1f4bbb76f5ec4f88cc262905c01fd3e6c53287d5986ed41d2864fe6f598f1790` |
| [scripts/ops/validate-aws-s3.sh](../../scripts/ops/validate-aws-s3.sh) | `35d0ff014729f939559c78e6f4ddb1309a21843206177d0d22a5e2763c70fff6` |
