# São Paulo S3 and network price inputs

Retrieved 2026-10-17 without AWS credentials. Region: `sa-east-1`, South America (São Paulo). Currency: USD public list prices. These are cost-model inputs, not an AWS quote or confirmation of throughput/recovery capacity. Taxes, exchange rates, discounts, credits, support, and negotiated rates are excluded.

Evidence is retained in [the parsed catalog subset](sao-paulo-storage-network-prices-2026-10-17.json), including SKU, price dimensions, effective dates, catalog version, and original URLs. S3 catalog publication: **2026-10-15T17:35:40Z**; AWSDataTransfer publication: **2026-09-16T13:22:08Z**.

## S3 Standard

For SKU `ZNX83R5ZU4NXKACG`, usage `SAE1-TimedStorage-ByteHrs`:

- First **51,200 GB-month**: **$0.0405 per GB-month**.
- Next tier, **51,200–512,000 GB-month**: **$0.039 per GB-month**.
- Above **512,000 GB-month**: **$0.037 per GB-month**.

PUT/COPY/POST/LIST SKU `EFD42GHQBGKAPBHU`, `SAE1-Requests-Tier1`: **$0.000007/request = $0.007/1,000 requests**.

GET/other Tier-2 SKU `BRDE588JVSNAD298`, `SAE1-Requests-Tier2`: **$0.00000056/request = $0.00056/1,000 requests**.

These are marginal storage tiers, not one rate applied to the entire volume after crossing a boundary. [Official regional S3 price catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/sa-east-1/index.json).

For monthly average stored volume `V` in AWS storage GB:

`storage USD = 0.0405 × min(V,51200) + 0.039 × max(min(V,512000)−51200,0) + 0.037 × max(V−512000,0)`.

For 30 days, `N` messages/day, one object/message, one PUT and one GET each: `requests USD = 30 × N × (0.000007 + 0.00000056)`. Thus requests alone are $2,268 at 10M/day, $4,536 at 20M/day, or $6,804 at 30M/day. These are arithmetic scenarios, not measured FRY traffic. Retries, extra reads, HEAD/LIST, multipart operations, and derived artifacts add operations.

## Internet egress

AWSDataTransfer SKU `QKTPK3975YUWDU3Q`, usage `SAE1-DataTransfer-Out-Bytes`, source São Paulo, destination External, transfer type AWS Outbound:

- Catalog range **0–10,240 GB/month**: **$0.150/GB**.
- **10,240–51,200 GB/month**: **$0.138/GB**.
- **51,200–153,600 GB/month**: **$0.126/GB**.
- **153,600 GB/month upward**: **$0.114/GB**.

The S3-specific catalog contains a **zero-dollar outbound line** for this usage. Do not interpret it as free internet egress: the shared AWSDataTransfer catalog supplies the actual tiers. [Official regional transfer catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/sa-east-1/index.json).

The pricing page grants **100 GB/month internet egress free**, aggregated across AWS services and regions excluding China/GovCloud; this is one shared allowance, not one per workload/service. Internet ingress is free. S3-to-AWS-service transfer within the same region is free at the S3 transfer layer, including another account. Additional network services can still charge. [S3 transfer pricing](https://aws.amazon.com/s3/pricing/).

Consumer location materially changes the estimate. Same-region consumers need no S3 internet-egress component; on-premises or other external consumers do. A GET count alone does not specify downloaded bytes. If FRY proxies a download through EC2, count the actual outward EC2 traffic rather than inventing a second S3 internet transfer. Additional consumers, partial fetches, retries, and IMAP rereads change outbound volume.

## Regional/AZ traffic

AWSDataTransfer SKU `YR2FH85AY9B4PG8R`, usage `SAE1-DataTransfer-Regional-Bytes`: **$0.010/GB**, described as regional data transfer in/out/between EC2 AZs or using elastic IPs or ELB. It is a rate for applicable charged usage, not a blanket rate for every regional byte. The same catalog also contains zero-priced `SAE1-DataTransfer-xAZ-In-Bytes` and `...-Out-Bytes` dimensions; a usage label without its service/path context is insufficient to price an architecture. [Transfer catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/sa-east-1/index.json).

For example, AWS's architecture guidance identifies cross-AZ EC2-to-RDS paths that charge both endpoints at $0.01/GB; one GB crossing such a path can therefore produce $0.02 combined. It also identifies free managed Multi-AZ replication paths. Model each chosen service/path separately rather than multiplying all mail bytes by a generic replication factor. NLB, NAT, endpoints, and database-specific exceptions need their own applicable rules. [AWS transfer-cost examples](https://aws.amazon.com/blogs/architecture/exploring-data-transfer-costs-for-aws-managed-databases/).

## Units and built-in durability

AWS explicitly defines **S3 storage GB as 2^30 bytes (GiB)** and storage TB as 2^40 bytes. If scenarios use decimal 100 KB/250 KB/500 KB message sizes, first multiply by 100,000/250,000/500,000 bytes, then divide by 1,073,741,824 for S3 storage billing. Do not treat decimal TB as an equal count of AWS storage TB. Transfer catalog units are labeled GB and the tier boundaries use 1,024 multiples; the retrieved storage definition explicitly covers storage. If applying the same binary conversion to transfer estimates, state that modeling convention rather than claiming a separately verified universal transfer-unit rule. [S3 unit definition](https://aws.amazon.com/s3/pricing/).

S3 Standard stores data across at least three AZs and is designed to retain it after loss of an entire AZ. **Do not multiply billed S3 Standard object storage by three** to represent AWS's internal AZ redundancy. Additional application-created buckets/copies or versions are separately stored usage. This does not prove FRY's metadata/queue durability or five-minute recovery. [S3 durability](https://docs.aws.amazon.com/AmazonS3/latest/userguide/DataDurability.html).

Use steady-state average occupancy for a mature 30-day retention window; a new installation's first month ramps up unless history is imported. The actual retained lifetime may exceed the configured duration because of grace periods or delayed deletion. No storage backend has been selected by this price collection.
