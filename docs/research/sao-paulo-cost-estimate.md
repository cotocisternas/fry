# FRY — first São Paulo cost estimate

Public USD list prices checked **2026-10-17**, region **sa-east-1**. No AWS account or infrastructure was created. The user chose Brazil and reports a mix of AWS and external consumers; the fraction outside AWS is unknown. This is a reproducible **modeled subtotal**, not a complete quote, an architecture decision, or a demonstrated throughput/recovery configuration.

## Result under explicit assumptions

At **20 million complete messages/day**, one S3 object per message, one PUT and one full GET per message, 50% of downloaded bytes sent to external consumers, and a **30-day effective stored footprint**, the estimates are:

| Mean encoded email size | Stored email bytes | Modeled monthly subtotal |
|---|---:|---:|
| 100 KB | 60 TB (54.6 TiB) | **$18,826** |
| 250 KB | 150 TB (136.4 TiB) | **$29,506** |
| 500 KB | 300 TB (272.8 TiB) | **$46,879** |

KB/TB in workload descriptions are decimal; S3 billed storage is converted to binary GB. These sizes are scenarios, not measured averages. The reported “usually below 1 MB” does not establish any of them. The occasional tentative 200 MB case affects streaming and peak memory but cannot establish an average.

Use 30 days of message traffic and 730 hours of continuously provisioned infrastructure. Effective stored footprint means the union of live mailbox mail and recovery-protected mail, without double counting common bytes. **Thirty days is not an expiry policy for visible messages.** Messages left in mailboxes, imported history, and grace periods can increase occupancy beyond the scenario. This is steady-state occupancy, not an empty first-month ramp-up.

## The 250 KB example, by component

| Component | USD/month |
|---|---:|
| S3 Standard, 150 decimal TB average stored | 5,525 |
| 600 million PUTs and 600 million GETs | 4,536 |
| Internet downloads, 75 decimal TB | 10,175 |
| One NLB, hourly plus processed-byte dimension | 2,500 |
| Illustrative compute, database, disks, fixed network | 6,770 |
| **Modeled subtotal** | **29,506** |

For the same 250 KB scenario, all consumer downloads staying in the same AWS region removes the internet-egress component, yielding approximately **$19,331**; all downloads going outside AWS yields approximately **$38,956**. This holds the other assumptions constant and does not mean every internal network path is free. Additional full downloads multiply byte traffic and GETs; IMAP partial fetches and caches require measurement.

S3 unit prices, tier arithmetic, and transfer charges are documented in the [storage/network research](sao-paulo-storage-network-prices.md), with original [S3 catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonS3/current/sa-east-1/index.json) and [transfer catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSDataTransfer/current/sa-east-1/index.json). Storage redundancy across S3 Standard's availability zones is included in its rate; do not multiply stored bytes by three for AWS's own replicas.

## What the illustrative $6,770 infrastructure line buys

This example prices six EC2 `m7i.2xlarge` workers, notionally two per AZ across three AZs, and a separate RDS PostgreSQL Multi-AZ DB instance. **Neither this hardware choice nor three zones has been selected, and neither has been load-tested for FRY.** A single hot inbox, retained metadata, concurrent sessions, and post-failure capacity may require a different database layout or substantially more resources.

- Six Linux on-demand workers, each 8 vCPU/32 GiB, at $0.6426/hour: **$2,814.59**.
- Six 100 GiB gp3 worker disks at $0.152/GiB-month: **$91.20**. These are not durable mail storage.
- EKS standard-support cluster at $0.10/hour: **$73**. No Auto Mode or provisioned-control-plane premium assumed.
- RDS PostgreSQL `db.r7g.2xlarge` Multi-AZ with one standby: $3.04167/hour plus 3,072 GiB gp3 at $0.438/GiB-month: **$3,565.96**. The rate already includes the standby; do not double it. No added IOPS/throughput assumed.
- Three zonal NAT gateways' fixed fees at $0.093/hour: **$203.67**, excluding data processing.
- Six public IPv4 addresses, modeled as three NLB plus three NAT addresses, at $0.005/hour: **$21.90**.

Evidence: [extracted compute/NLB/EKS prices](sao-paulo-compute-prices-2026-10-17.json), [EC2 regional catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/sa-east-1/index.csv), [EKS pricing](https://aws.amazon.com/eks/pricing/), [RDS research](sao-paulo-database-prices.md), [IPv4/NAT pricing](https://aws.amazon.com/vpc/pricing/).

## Network modeling and material omissions

The NLB example uses $0.034/hour plus $0.00825 per NLCU-hour, with the processed-byte dimension modeled as one incoming complete copy plus one outgoing fetched copy per message. This assumes direct SMTP ingest through the NLB, and TCP forwarding with TLS handled by FRY; connections, protocol overhead, retransfers, or additional load balancers can add costs. Actual NLCUs use the largest applicable hourly dimension; this is not a measured bill. [NLB catalog](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AWSELB/current/sa-east-1/index.json), [NLCU definition](https://aws.amazon.com/elasticloadbalancing/pricing/).

For conservative budgeting, the model converts networking bytes to **decimal GB**, while S3 storage uses documented binary GB. Transfer catalogs label units GB without independently establishing their byte definition here; reconcile actual metering during a deployment spike. It applies the shared 100 GB internet allowance by subtracting 100 before tiering; existing account usage and exact tier treatment can slightly alter the result.

Not included in the subtotal:

- **Variable NAT processing:** $0.093 per modeled GB. Every additional 10 decimal TB traversing NAT adds about **$930**, before other transfer charges. External IMAP collection and outgoing SMTP can make this significant. Their volumes are unknown. Incoming SMTP/NLB response traffic is not automatically NAT traffic. The example assumes S3 access via a gateway endpoint, avoiding NAT charges for S3 transfers. [AWS NAT and gateway-endpoint pricing](https://aws.amazon.com/vpc/pricing/).
- Cross-AZ network charges where applicable, connectivity for other-region AWS consumers, VPN/Direct Connect, or paid interface endpoints. S3 same-region transfers and managed database replication have specific exemptions; price actual paths rather than multiplying all bytes indiscriminately.
- Outgoing submission volumes, external relay fees, additional recipients, and retry traffic. These are additional to the incoming-mail scenario.
- Imported historical data beyond the modeled footprint, backup/snapshot excess, log/audit ingestion and retention, monitoring, DNS, image registries, KMS requests if selected, and additional queue/cache services if the architecture needs them.
- Extra database capacity/performance/shards, peak worker scaling, development/staging, migration overlap with old servers, and an additional web load balancer if used.
- Taxes, exchange-rate conversion to BRL, support plans, and engineering/operations labor. No savings commitment, spot discount, free-tier credit, or negotiated discount assumed.

## How to refine it

Measure mean encoded message size and short-window byte rates; count live/history bytes and mailbox deletion age; measure total consumer downloads and the external fraction; quantify outgoing and IMAP-collection traffic. Then benchmark a concentrated 500,000-message inbox batch and AZ failure with surviving capacity. Price catalog rates do not prove the no-loss/five-minute recovery target.

The [Python model](sao-paulo-cost-model.py) generates [scenario data](sao-paulo-cost-model.json). The [interactive calculator](sao-paulo-cost-calculator.html) exposes the same assumptions and keeps the illustrative infrastructure fixed so its arithmetic is reproducible, not automatically sized.
