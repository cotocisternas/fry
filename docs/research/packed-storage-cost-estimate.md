# FRY — replicated hot storage and packed-object cost model

Public USD list prices checked **2026-10-17**, AWS São Paulo (`sa-east-1`). No resources or AWS account were created. The [Python model](packed-storage-cost-model.py) and [generated scenarios](packed-storage-cost-model.json) leave the [individual-object baseline](sao-paulo-cost-estimate.md) unchanged. These are comparable modeled subtotals, **not a complete quote or validated production sizing**.

## Result

At 20 million messages/day, 30 days of effective stored mail, and half of one full download/message going outside AWS:

- **100 KB mean:** earlier individual-object subtotal **$18,826/month**; packed-storage subtotal **$17,586/month**, about **$1,241 lower**.
- **250 KB mean:** earlier **$29,506/month**; packed **$30,525/month**, about **$1,019 higher**.
- **500 KB mean:** earlier **$46,879/month**; packed **$51,664/month**, about **$4,785 higher**.

Packing sharply reduces S3 write requests, but **does not establish net savings**. In the 250 KB case, cross-AZ byte replication alone adds $3,000/month; extra storage workers, disks, and reclamation occupancy add more. The old subtotal omitted unspecified cross-AZ paths, while this one adds an explicit hot-replication path. This comparison is useful cost accounting, not proof that the earlier design had every necessary cost priced.

The means are hypothetical decimal encoded-email sizes, not the reported “usually under 1 MB” converted into a measurement. Thirty days means the union of live-mailbox and recovery-protected mail; it does not permit deleting messages still visible in clients. Imported history or long-lived mail can grow it.

## Inputs and formulas

The default workload is **600 million messages and 150 decimal TB/month**. Internet downloads stay **75 decimal TB/month**, priced exactly as in the earlier model; the same NLB byte assumptions and six-worker/database/fixed-network line are retained.

### Hot storage

Price **two durable hot copies in distinct AZs**, using three additional `m7i.2xlarge` storage workers, notionally one per AZ, beyond the earlier six general workers. Nine total workers is an arbitrary pricing scaffold, not a selected node count or measured capacity.

Provision hot data disks for `(6 hours outage buffer + 1 hour normal occupancy) × hourly original bytes × 2 copies × 1.30`. The default is **3.792 decimal TB before GiB rounding**, or **3,534 GiB total**, rounded to **1,178 GiB/worker** under equal placement. At $0.152/GiB-month, hot capacity is **$537.17/month**. Three workers at $0.6426/hour plus three 100 GiB root disks add **$1,452.89/month**, using 730 compute hours. [Dated compute evidence](sao-paulo-compute-prices-2026-10-17.json), [AWS EC2 regional catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/sa-east-1/index.csv).

The one-hour occupancy and 30% additional capacity are adjustable assumptions; neither was chosen by the user. Headroom must actually cover packing scratch space, repair, filesystem overhead, and operational free space. This aggregate calculation assumes healthy, balanced workers and **does not prove six-hour capacity after losing a worker/AZ**. Volume and instance bandwidth, durable-write latency, post-outage drain rate, uneven shards, and extra provisioned performance need measurement. gp3 included performance is finite; capacity in GiB alone does not establish throughput. [AWS EBS pricing](https://aws.amazon.com/ebs/pricing/).

### Packs and requests

Model **64 MiB target packs at 80% mean fill**: about 51.2 MiB payload or 215 mean-sized messages. Organization grouping and an age limit can reduce fill; 20% is also evaluated. Objects have their actual written length, so underfilled packs increase request counts **without padding storage to 64 MiB**. This is a candidate, not an approved format. Record framing overhead and the tentative 200 MB outlier distribution are not yet quantified.

Use one single-part PUT per new pack, streamed from a durable local file with bounded memory. Retries resend from disk; they do not require a full pack in RAM. The default assumes no failed-upload retries, with an adjustable retry fraction. Large originals outside this candidate pack size need a separately validated dedicated-object/streaming path; multipart operations and their request amplification are not silently included. The index stays in PostgreSQL, with **no additional index object per pack**. [Current FRY storage proposal](../design/replicated-hot-storage.md).

Default operations/month are about **2.794 million initial PUTs**, **279,397 compaction PUTs**, and **one verification HEAD per successful upload**. Reads receive **no cache credit**: **600 million range GETs**, one per message, remain priced. Compaction uses full-pack GETs separately. Actual IMAP partial fetches, rereads, and retries can increase requests. S3 supports a byte range in a GET, while HEAD returns metadata rather than body bytes; neither operation substitutes for application checksum validation. [S3 GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html), [S3 HeadObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html).

Implementation note (2026-10-17): the [current cold-read prototype](../validation/packed-objects.md) uses four GETs per nonempty read, including framing validation. At this model's 600 million cold reads/month and dated GET rate, that request line would be **$1,344/month**, **$1,008 more** than the one-GET illustration below. The saved baseline is unchanged for comparison; it must not be treated as pricing the current implementation. Hot-cache effects and actual client fetch patterns still require measurement.

At $0.007/1,000 PUTs and $0.00056/1,000 GET-class requests, the model uses HEAD at the Tier-2 rate. Original-pack PUTs cost **$19.56/month**, compared with the earlier **$4,200/month** individual-message PUTs. One GET/message remains **$336/month**. [Dated S3 evidence](sao-paulo-storage-network-prices-2026-10-17.json), [AWS S3 regional catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/sa-east-1/index.json), [AWS S3 request pricing](https://aws.amazon.com/s3/pricing/).

For the proposed late-worker protection, retirement writes a permanent **zero-byte tombstone to the same key**, rather than deleting the key. Price one conditional retirement PUT and one confirming HEAD per retired pack. Approximate steady-state pack retirements by successful initial-plus-compaction creations: **3.073 million/month**, adding **$23.23/month**. These are steady-state flows, not a forecast of exact first-month timing. Zero-byte bodies do not make catalog rows, keys, operational metadata, or their enumeration free; permanent tombstone counts grow without bound unless a separately proven reclamation protocol changes that. The model assumes a never-versioned bucket; retaining overwritten versions would invalidate the physical-byte cleanup and storage assumptions. The conditional protocol is specified separately, not verified by this pricing model.

### Compaction and deletion occupancy

Default live-byte rewrite volume is **10% of monthly input**, or **15 TB/month**. If input packs are 80% live when compacted, full-pack reads are **18.75 TB/month**, with roughly **349,246 compaction GETs**. Rewrites add **279,397 PUTs** at the candidate fill. Model one hour of old/new live-byte overlap: about **20.8 GB** average extra storage. Same-region S3 access through a gateway endpoint adds no modeled S3 transfer or NAT processing charge; compute, EBS I/O, and any cross-AZ worker traffic still matter. [AWS same-region transfer guidance](https://aws.amazon.com/blogs/architecture/overview-of-data-transfer-costs-for-common-architectures/), [AWS VPC gateway endpoint pricing](https://aws.amazon.com/vpc/pricing/).

For cleanup occupancy, illustrate **all bytes expiring at a smooth rate equal to new intake remaining for the full 24-hour deadline**: 5 TB additional dead bytes. This is a conservative upper illustration **only under that rate assumption**, not a bound under burst deletions, imported-history cleanup, or failed reclamation. Add it and overlap to 150 TB logical occupancy for **155.021 TB billed S3 occupancy**, about **$5,707/month**. No S3 storage credit is taken for the one-hour hot residency. AWS S3 Standard's internal AZ copies are included in its price; do not multiply this amount by two or three. [AWS S3 storage pricing](https://aws.amazon.com/s3/pricing/), [S3 durability](https://docs.aws.amazon.com/AmazonS3/latest/userguide/DataDurability.html).

The 10% rewrite rate **does not prove compliance with the 24-hour physical-removal requirement**. A nearly full live pack with one newly dead record can force copying almost the whole pack; repeated sparse deletions can rewrite live bytes many times. The model's 0%, 10%, 50%, and 100% sensitivities are cost inputs, not feasibility claims. Zero compaction meets cleanup only when whole-pack retirement suffices. Worst-case required copy bandwidth, temporary space, worker count, database index updates, and reader coordination need failure/workload validation.

### Cross-AZ replication

Price a private EC2 storage-worker-to-storage-worker byte stream across two AZs, charged at **$0.01/GB at each endpoint**, or **$0.02 per transferred GB combined**. This applies to FRY's explicit replica traffic, not S3's managed replication or RDS's exempt primary/standby replication. The regional catalog contains the $0.01 regional usage dimension; AWS describes cross-AZ EC2 charges and both endpoint charges. [Dated transfer evidence](sao-paulo-storage-network-prices-2026-10-17.json), [AWS regional transfer catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/sa-east-1/index.json), [AWS cross-AZ charging guidance](https://aws.amazon.com/blogs/networking-and-content-delivery/optimizing-data-transfer-costs-when-using-aws-network-load-balancer/).

One replica byte stream per incoming byte costs **150,000 decimal GB × $0.02 = $3,000/month**. As in the old model, networking uses an explicit conservative decimal-GB conversion; actual billing-unit reconciliation remains necessary. Baseline placement assumes receipt reaches its first writer locally, and packing reads local copies. The extra-stream parameter models aggregate additional cross-AZ traffic from frontend placement, hot reads, packing, repair, or other selected paths. Do not count it twice if a concrete topology already accounts for a path elsewhere.

## Sensitivities and limitations

At 250 KB, holding all other assumptions fixed:

- Pack fill **20% / 50% / 80% / 100%**: **$30,665 / $30,553 / $30,525 / $30,516**. Request prices barely change the total; low fill can still harm metadata and operations.
- Rewritten live bytes **0% / 10% / 50% / 100%** of monthly input: **$30,520 / $30,525 / $30,546 / $30,572**. This small dollar change does not price additional CPU or storage performance needed to sustain the larger rewrite load.
- Additional cross-AZ streams **0 / 0.5 / 1** times monthly original bytes: **$30,525 / $32,025 / $33,525**.
- Combining 20% fill, 100% rewrites, and one extra cross-AZ stream gives **$33,831/month**, still excluding any additional compute/performance those conditions require.

Unpriced items remain material: ALB/admin web serving, logs/audits/metrics, per-record database storage/indexes and compaction updates, permanent tombstone catalog growth, extra EBS IOPS/throughput, AZ repair and failover capacity, byte framing/protocol overhead, upload failures, outbound mail and relay fees, variable NAT processing, other-region consumers, backups, migration overlap, taxes, support, and staff. Additional metadata cross-AZ traffic is not assumed equal to email size. The earlier RDS/worker example is retained for comparability, not endorsed as sufficient for hundreds of millions of rows or a hot inbox.

Run `python docs/research/packed-storage-cost-model.py` to regenerate JSON and execute built-in arithmetic checks. Checks cover baseline preservation, 150 TB traffic/75 TB egress, replica/headroom capacity, GiB rounding, both-endpoint charges, underfill without storage padding, zero-rewrite behavior, retirement requests, and invalid inputs. They validate this calculator's arithmetic, not FRY durability, performance, or AWS metering.
