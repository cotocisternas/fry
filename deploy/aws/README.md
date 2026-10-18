# Disposable S3 validation in São Paulo

This bundle prepares the first real-service check of FRY's packed-object adapter.
It has **not run against AWS**. The reviewed local fixtures do not prove S3
conditional-write or IAM behavior. The bundle uses one new bucket and two
temporary-role identities; it creates no EKS cluster, database or mail listener.

The operator needs an AWS account, an authenticated provisioning profile, and an
existing same-account IAM role that may assume the two application roles. No
access keys belong in this repository. Authenticate through your existing
temporary-credential workflow, such as IAM Identity Center. The source profile
must have the necessary assume-role permission in addition to the generated
roles' trust policy. See the [primary-source research](../../docs/research/aws-s3-validation-bundle.md).

## Review and provision separately

[`s3-validation.json`](s3-validation.json) is a CloudFormation template for:

- One uniquely named `fry-validation-ACCOUNT-sa-east-1-SUFFIX` bucket with
  Bucket owner enforced ownership, Block Public Access and SSE-S3 encryption.
  Versioning and lifecycle are omitted deliberately.
- Producer and retirement roles trusted by the specified operator role, with
  access confined to this bucket and the `packs/v1/` object prefix.
- Explicit denies for unconditional writes, producer overwrites, application
  deletion, object ACL/tag changes and bucket-configuration changes. Retirement
  remains a trusted conditional writer: IAM does not enforce its empty-body or
  `fry-retired` user-metadata contract.

This example is a **provisioning action**, separate from the validation runner.
Replace the placeholders only after selecting the account and reviewing the
template. It creates IAM roles and incurs AWS request/storage charges.

```sh
aws cloudformation deploy \
  --profile OPERATOR_PROFILE --region sa-east-1 \
  --template-file deploy/aws/s3-validation.json \
  --stack-name fry-s3-validation-UNIQUE_SUFFIX \
  --parameter-overrides OperatorRoleArn=OPERATOR_IAM_ROLE_ARN RunSuffix=UNIQUE_SUFFIX \
  --capabilities CAPABILITY_IAM

aws cloudformation describe-stacks \
  --profile OPERATOR_PROFILE --region sa-east-1 \
  --stack-name fry-s3-validation-UNIQUE_SUFFIX \
  --query 'Stacks[0].Outputs'
```

The suffix must contain 8–16 lowercase letters/digits. Use a new stack/bucket;
the template does not adopt existing mail storage. Only JSON parsing and local
review have been performed so far, not CloudFormation deployment or AWS policy
validation. The template's bucket has `DeletionPolicy: Retain`, so deleting the
stack alone does not clean up the retained bucket or its objects.

## Configure and run the probe

Create two local AWS shared-config profiles using the output role ARNs and the
operator's temporary-credential profile as their source:

```ini
[profile fry-validation-producer]
role_arn = PRODUCER_ROLE_ARN
source_profile = OPERATOR_PROFILE
region = sa-east-1

[profile fry-validation-retirement]
role_arn = RETIREMENT_ROLE_ARN
source_profile = OPERATOR_PROFILE
region = sa-east-1
```

Copy [`examples/aws-s3-validation.json`](../../examples/aws-s3-validation.json)
to a private local file. Replace its bucket and expected owner with stack outputs;
keep its two profile names aligned with your local configuration. It contains no
credential values. Do not add endpoint overrides or production bucket names.

Create a private directory for the manifest, then explicitly acknowledge the
exact bucket name:

```sh
mkdir -m 700 /ABSOLUTE/PRIVATE/validation-run
./scripts/ops/validate-aws-s3.sh /ABSOLUTE/config.json EXACT_BUCKET \
  /ABSOLUTE/PRIVATE/validation-run/manifest.json
```

The runner rejects partial opt-in, configuration drift from this profile, an
existing manifest, the wrong region/owner/tag, non-temporary credentials, or
profiles resolving to the same role. It freezes the actual S3 credential values
for its bounded run and identifies those same sessions through STS. Profile names
alone are not identity evidence. No raw credentials are logged or persisted.
SDK service-endpoint overrides are ignored before credential loading; operator
profiles, credential-process programs, proxies and host CA trust remain trusted.

Before writing, it creates and fsyncs a private manifest listing all five possible
keys, the namespace, bucket/account/region and actual role ARNs. The synthetic
body is under 4 KiB. The run checks creation, SHA-256/ETag metadata, exact range
bytes, duplicate/stale conflicts, existing/missing retirement, a joined race,
late-producer refusal and sampled forbidden writes/deletes. Denied operations
must return both HTTP 403 and `AccessDenied`; a failed precondition is insufficient.
Settings are observed again at completion. The test context is five minutes;
SDK request/retry and test-process bounds also apply.

**The live probe never changes policies, enables versioning, creates lifecycle
rules or provisions resources.** Those mutation-denial tests in the research's
broader candidate matrix remain separate work. It samples permissions using the
configured identities, not every possible principal or request shape. It does
not establish future policy stability, production erasure, throughput or recovery
guarantees. Local unit tests of these gates are distinct from real AWS evidence.

## Preserve evidence and clean up with the operator identity

Successful validation leaves three empty marker objects and the manifest. A
failed run can leave any of the five listed keys as an image, a marker or absent.
Keep the manifest and output; never delete them just because the probe failed.
The success message describes the contract probe only, not completed cleanup.

1. Stop and join the runner and every application writer using these two roles.
   Prevent new sessions and revoke their write access using the provisioning
   identity before removing marker objects. Do not grant application DeleteObject
   permission to make cleanup convenient.
2. Use the provisioning identity to inventory the **exact dedicated bucket** and
   remove only its disposable synthetic contents. Check it is empty; if it has
   unexpected content or settings, resolve that discrepancy before proceeding.
3. Delete the dedicated CloudFormation stack and wait for successful deletion of
   its policies and roles. Its bucket is retained by design.
4. Delete that empty retained bucket with the provisioning identity and verify
   absence. Record cleanup separately from the contract-test result.

No cleanup command runs automatically. A future automation can consume the
manifest, but must independently verify ownership, scope and stopped writers;
the manifest itself is not authority to delete arbitrary S3 objects.
