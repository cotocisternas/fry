# São Paulo RDS PostgreSQL reference costs

Checked 2026-10-17. These are public USD on-demand list prices for `sa-east-1`, usable without an AWS account. They illustrate a possible metadata database, not an architecture selection or a claim that this configuration handles FRY's workload.

## Reference deployment and rates

Use an RDS PostgreSQL **Multi-AZ DB instance with one standby** as the reference. This is a primary plus a synchronously replicated standby in a different availability zone. The standby does not serve reads. It is distinct from a Multi-AZ DB cluster with two readable standbys across three zones. [AWS deployment documentation](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZSingleStandby.html), [AWS PostgreSQL pricing](https://aws.amazon.com/rds/postgresql/pricing/).

The regional AWS price catalog, publication `2026-10-06T22:40:50Z`, lists these PostgreSQL rates effective `2026-10-01T00:00:00Z`:

- `db.r7g.2xlarge`, 8 vCPU and 64 GiB RAM, Multi-AZ: **$3.04167/hour**. SKU `XHJC6R7HWJVU5K6J`, usage `SAE1-Multi-AZUsage:db.r7g.2xl`. This is the Multi-AZ deployment rate including the standby; do not multiply it by two. The separate Single-AZ SKU `D6JZWQD944PRDFSS` is $1.52004/hour, and is not the rate used below.
- Multi-AZ gp3 storage: **$0.438 per provisioned GB-month**. SKU `76QZ3RFK75ZTYQZE`, usage `SAE1-RDS:Multi-AZ-GP3-Storage`. Apply this Multi-AZ rate to the provisioned logical size once; do not double storage again for the standby.
- Additional Multi-AZ gp3 IOPS: **$0.076 per IOPS-month**. SKU `BF7BP93TH85E5MY8`.
- Additional Multi-AZ gp3 throughput: **$0.305 per MiB/s-month**. SKU `SCVCNBZY5D8BM77C`.

Source: [AWS regional RDS price catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonRDS/current/sa-east-1/index.json). The small [extracted evidence file](sao-paulo-database-prices-2026-10-17.json) preserves product attributes, complete price dimensions, effective dates, and catalog version for reproduction. The current URL can change over time.

## Illustrative monthly arithmetic

Using **730 compute hours/month**, one such Multi-AZ deployment costs $3.04167 × 730 = **$2,220.42/month** in instance charges.

- **1 TiB provisioned storage:** 1,024 × $0.438 = **$448.51/month**; instance plus storage **$2,668.93/month**.
- **3 TiB provisioned storage:** 3,072 × $0.438 = **$1,345.54/month**; instance plus storage **$3,565.96/month**.

These are arithmetic scenarios, not measured database sizing. RDS exposes provisioned sizes in GiB, while its catalog labels this billing unit `GB-Mo`; the examples consistently use 1,024 or 3,072 provisioned units. PostgreSQL gp3 at these sizes includes a baseline of 12,000 IOPS and 500 MiB/s; AWS charges for provisioned performance above that baseline. An extra 10,000 IOPS would therefore add $760/month at the listed Multi-AZ rate. Storage performance limits do not guarantee the database instance or application achieves that throughput. [AWS storage documentation](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/CHAP_Storage.html).

Totals exclude extra performance, excess backup storage, monitoring/logs, network charges, taxes, support, and any Extended Support charges for old engine versions. No reservation or savings discount is assumed. They also exclude email-object storage and the rest of FRY. [AWS PostgreSQL pricing](https://aws.amazon.com/rds/postgresql/pricing/).

This estimate does not validate FRY's no-accepted-mail-loss/five-minute availability-zone recovery target. Database durability is only one part of the acceptance path; message bytes, metadata commits, mailbox state, queues, and application failover must be designed and tested together. Workload, indexes, mailbox sizes, and recovery measurements must determine eventual database sizing.
