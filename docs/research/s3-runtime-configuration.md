# S3 runtime configuration

Investigated 2026-10-17 against official AWS documentation and FRY's pinned SDK source. Scope: configure the existing adapter; no AWS credentials, bucket provisioning, or IAM deployment was performed.

## Concrete runtime settings

Require a region and bucket. For AWS, require the expected bucket owner's 12-digit account ID; leave the endpoint unset. Provide independent producer and retirement credential selections. For compatible storage, allow an explicit endpoint plus path-style addressing, require HTTPS with normal certificate/hostname verification, and permit HTTP only under an explicit fixture switch with a numeric loopback host. Reject URL credentials, query, fragment, and unexpected base paths. These endpoint/TLS restrictions are recommended FRY policy, not guarantees supplied by the AWS SDK. The SDK supports `BaseEndpoint` and may insert the bucket into its hostname; `UsePathStyle=true` avoids that for local/compatible services. Keep the default S3 endpoint resolver rather than replacing it. [AWS endpoint configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html).

Minimal construction, with separate profile names configured by the operator:

```go
producerConfig, err := config.LoadDefaultConfig(ctx,
    config.WithRegion(region), config.WithSharedConfigProfile(producerProfile))
// Check err; load retirementConfig separately with retirementProfile.
producer := s3.NewFromConfig(producerConfig, func(o *s3.Options) {
    // Set these only after validating an explicitly configured override:
    // o.BaseEndpoint = aws.String(endpoint)
    // o.UsePathStyle = true
})
// Build retirement with the same bucket/region/endpoint settings and its own config.
store, err := objectstore.NewS3(ctx, producer, retirement, bucket, expectedOwner)
```

At inspection, [go.mod](../../go.mod) pinned core SDK `v1.47.3`, S3 `v1.114.3`, and smithy `v1.28.5`; the `config` module was not yet a dependency. Add and pin a compatible `github.com/aws/aws-sdk-go-v2/config` module in the implementation change. Existing S3 options support `BaseEndpoint`, `UsePathStyle`, `RequestChecksumCalculation`, and `ResponseChecksumValidation`. [Pinned S3 options source](https://github.com/aws/aws-sdk-go-v2/blob/service/s3/v1.114.3/service/s3/options.go).

## Credentials in AWS and on premises

`LoadDefaultConfig` discovers environment credentials (including web identity), shared files, ECS task credentials, then EC2 instance credentials. Use `AWS_REGION` or an explicit region; the SDK has no universal default region. Providers are cached with rotation support. Explicit shared-profile selection is preferable to letting two clients independently choose the same default identity. [Go v2 configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html).

A profile may assume a role using `role_arn` with `source_profile` or `credential_source`; choose distinct final roles for producer and retirement. Two profile labels do not prove different effective principals. The config resolver treats an explicitly selected shared profile separately from its ordinary environment-credential fallback, so do not describe explicit profiles as an unconditional overlay on every default-chain source. [Assume-role settings](https://docs.aws.amazon.com/sdkref/latest/guide/feature-assume-role-credentials.html), [official resolver source](https://github.com/aws/aws-sdk-go-v2/blob/main/config/resolve_credentials.go).

EKS IRSA uses web identity token exchange with STS and automatic temporary-credential renewal. Earlier credentials in the default chain can shadow IRSA; remove ambient static keys. One pod's default chain is one identity: use explicit role providers/profiles, or separate deployment identities/processes, to separate producer from retirement permissions. [EKS IRSA SDK behavior](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts-minimum-sdk.html).

For on-premises hosts accessing AWS, a shared profile can invoke the IAM Roles Anywhere credential helper through `credential_process`, yielding renewable temporary credentials without embedding access keys in FRY configuration. Its trust anchor, certificate/key protection, profile, and role policy remain deployment responsibilities. For a compatible service with its own keys, use separately provisioned credential profiles and its documented signing region. [IAM Roles Anywhere helper](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/credential-helper.html).

## Required storage and permission behavior

The [adapter](../../internal/objectstore/s3.go) uses single-part PUT, explicit base64 SHA-256, `If-None-Match: *` for creation, and conditional `If-Match` replacement for retirement. Retirement writes an empty permanent marker with `fry-retired=true`; it never calls DELETE. AWS conditional PUT returns 412 on a failed precondition and can return 409 on concurrent conflict; FRY translates both to conflict. A supplied SHA-256 is independent of the SDK's newer automatic CRC defaults. Do not weaken FRY's explicit checksum requirement for compatibility. [PutObject contract](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html), [SDK checksum settings](https://docs.aws.amazon.com/sdkref/latest/guide/feature-dataintegrity.html).

HEAD must return SHA-256 when checksum mode is enabled. ETag is an opaque conditional token, not FRY's content digest. Expected owner mismatch fails with 403. Missing-key HEAD can also return 403 instead of 404 without `s3:ListBucket`; retirement needs a policy permitting reliable missing-key detection, tested against its actual permissions. For general purpose SSE-KMS objects, checksum retrieval requires `kms:Decrypt`; provisioning must account for encryption/key policy. [HeadObject permissions and checksum contract](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html).

Recommended permissions on the intended `packs/v1/` object namespace: producer reads and conditional creation only; retirement reads and conditional marker PUT. Grant neither principal object deletion. Retirement additionally needs bucket `GetBucketVersioning`, `GetLifecycleConfiguration`, and the missing-key visibility described above. The adapter rejects enabled **or suspended** versioning and any lifecycle rules at startup because historical bytes and marker expiry violate its contract. These are current observations; policy must prevent later administrative changes. [Adapter checks](../../internal/objectstore/s3.go).

Use bucket-policy enforcement of producer `s3:if-none-match` conditional writes, with role-specific exceptions allowing retirement's `s3:if-match` writes. AWS documents these condition keys. Review all other identity/resource policies too: merely placing a conditional Allow beside a broader Allow does not establish the intended restriction. FRY does not use multipart/copy, so do not grant these unnecessarily. [AWS conditional-write enforcement](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes-enforce.html).

## What the adapter cannot prove

Separate client objects, profile names, a successful credential retrieval, or expected owner checks cannot prove distinct effective IAM principals, producer inability to overwrite/delete, permanent-marker protection, or absence of another administrator capable of changing lifecycle/versioning/policies. Startup checks cannot establish these properties for the future. A compatible endpoint may ignore owner headers or implement weaker conditions/checksums. Require independent policy review and real-service permission/conditional-race tests before relying on the retirement guarantee; the existing [synthetic HTTP fixture](../../internal/testutil/s3fixture) demonstrates wire behavior only. These are conclusions from the adapter boundary, not claims that this repository has verified a cloud deployment.
