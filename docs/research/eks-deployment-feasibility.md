# EKS deployment feasibility

Checked 2026-10-17. AWS is selected for the first rollout; Kubernetes is preferred if feasible. The user subsequently selected one AWS region across multiple availability zones for the first rollout. EKS and its compute mode remain candidates. The user subsequently set the first-profile target to no accepted-mail loss and mail reception/mailbox access restored within five minutes after one whole availability-zone failure; this remains unvalidated. No infrastructure was created.

## SMTP and IMAP can use Kubernetes TCP services

AWS documents Network Load Balancers routing layer-4 traffic to EKS workloads, including direct pod IP targets with supported networking. A Kubernetes `Service` of type `LoadBalancer` can provision an NLB using the AWS Load Balancer Controller; EKS Auto Mode is another documented management path. Internet-facing versus internal exposure must be configured deliberately. [EKS network load balancing](https://docs.aws.amazon.com/eks/latest/userguide/network-load-balancing.html).

**Candidate:** expose FRY SMTP/IMAP services through NLB TCP listeners and run application processes in pods. The previously researched public SMTP 25 → backend 2525 mapping remains a candidate requiring account/network verification. Outbound public-port-25 restrictions are not removed by running inside Kubernetes. This establishes a supported network building block, not demonstrated FRY interoperability or throughput.

## Regional availability is distinct from regional disaster recovery

Each EKS control plane spans three Availability Zones **within one AWS Region**, with at least two API servers and three etcd instances. AWS replaces failed control-plane instances. These guarantees concern the managed Kubernetes control plane; they do not establish durability of FRY messages or availability after the entire region fails. [EKS architecture](https://docs.aws.amazon.com/eks/latest/userguide/eks-architecture.html).

**Design implication:** a regional profile needs explicit worker, networking, and persistence failure coverage. A resilient multi-region profile needs independently recoverable compute/control infrastructure and durable data outside the failed region, plus traffic failover. The first profile is now single-region across multiple availability zones; a single multi-AZ EKS cluster must not be described as satisfying whole-region recovery.

## Candidate separation and validation

Keep durable originals, mailbox metadata, and pending work independent of an individual pod's lifetime. Database/object-store implementations and whether their services run inside or outside Kubernetes remain open. Pod restart alone is not evidence of durable SMTP acceptance. IMAP sessions and mailbox ownership retain state and coordination requirements; adding pod replicas does not make IMAP stateless.

NLB supports client-IP preservation and deregistration/draining controls. Test those alongside application shutdown, readiness, long-lived IMAP connections, STARTTLS, interrupted SMTP uploads, large messages, and client reconnection. [NLB target attributes](https://docs.aws.amazon.com/elasticloadbalancing/latest/network/edit-target-group-attributes.html).

A deployment spike must prove mailbox-owner takeover, stable IMAP identifiers, retry-safe pending work, and recovery under the selected profile. Validate surviving capacity and the applicable recovery target together; neither Kubernetes scheduling nor control-plane availability supplies that proof automatically.
