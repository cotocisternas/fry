# AWS SMTP feasibility for FRY

Checked 2026-10-17 against primary AWS documentation. AWS was a candidate during this investigation; the user subsequently selected it for the first rollout, with Kubernetes preferred if feasible. The initial profile is one region across multiple availability zones; EKS and the specific region remain unselected. FRY intends to operate its own SMTP receiver and IMAP storage, durably queue outgoing submissions, and retry delivery to an installation-default relay or an organization's configured last-mile MTA.

## AWS does not impose a blanket SMTP prohibition

The EC2 guide identifies a default restriction on **outbound port 25 to public addresses**, with exceptions for specified private address ranges/VPC destinations. It provides a request process for removal. This is distinct from a ban on running an SMTP server or using SMTP over another supported port. Do not generalize the public-port restriction to every private-network SMTP connection. [EC2 restrictions](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-resource-limits.html#port-25-throttle).

The AWS Knowledge Center article primarily describes outbound restrictions, but also contains an ambiguous note saying inbound SMTP restrictions operate at the EC2 instance level and inbound Network Load Balancer traffic is not blocked. Consequently, this investigation **does not establish that direct inbound EC2 port 25 is universally unrestricted**. The chosen account, region, target type, and network path need verification. The article says removal requests are submitted separately per region; approval must not be assumed. [AWS Knowledge Center](https://www.repost.aws/knowledge-center/ec2-port-25-throttle).

## Candidate receiving path

**Inference to test:** internet SMTP on TCP 25 → internet-facing Network Load Balancer TCP listener → private FRY target on TCP 2525.

AWS documents TCP listeners on ports 1–65535, with forwarding to target groups. Target-group protocol/port are configured independently, and individual target ports can be overridden. Together with the Knowledge Center's NLB statement, this supports the proposed mapping; it is not a tested deployment or a guarantee covering every account/configuration. [NLB listeners](https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-listeners.html), [NLB target groups](https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-target-groups.html).

In this candidate, FRY handles the SMTP dialogue and STARTTLS through TCP forwarding. Public senders still connect on port 25; choosing backend port 2525 does not require changing their behavior. Validation must include DNS/MX addressing, security groups and routing, target health, sender-IP preservation for later authentication/spam checks, STARTTLS, connection timeouts, and slow large-message uploads. NLB listener defaults include a 350-second TCP idle timeout, which must be considered alongside FRY's SMTP timeouts. A reachable listener does not prove durable receipt or the active deployment profile's failover guarantee.

## Outbound relay connections

The user confirmed that the existing relay/last-mile MTAs support authenticated SMTP on port 587 or 465. For that reported set of destinations, the proposed outbound path does not depend on an outbound public-port-25 exception. Future organization-specific MTAs still need capability checks.

**Candidate:** connect to the selected third-party MTA using authenticated SMTP on 587 with STARTTLS or 465 with implicit TLS, if that MTA supports it. Those connections avoid the specific public-port-25 restriction; normal routing/firewall access, authentication, TLS, size limits, and destination policies still apply. AWS itself documents alternate SMTP ports as an option for avoiding its port-25 throttle. This does not require using SES. [AWS SMTP connection guidance](https://docs.aws.amazon.com/ses/latest/dg/smtp-connect.html).

If an organization requires a public MTA accepting only port 25, obtain and verify restriction removal for the deployment region before promising compatibility. A documented private-network exception may instead apply to a privately connected MTA. No organization-specific MTA should silently fall back to the shared relay. FRY retains queue/retry ownership; AWS connectivity and downstream acceptance do not imply final recipient delivery.

## SES is optional and has different limits

SES SMTP/v2 sending has a documented 40 MB complete-message limit after encoding. Its documented inbound-to-S3 limit is also 40 MB. Therefore SES cannot simply be substituted if FRY must pass approximately 200 MB complete emails. That figure remains a tentative test case, not an agreed maximum. These SES quotas are not a documented size cap on FRY's own TCP receiver. [SES quotas](https://docs.aws.amazon.com/ses/latest/dg/quotas.html).

No account-specific connectivity test, restriction-removal request, infrastructure deployment, or throughput/large-message benchmark was performed. Feasibility remains conditional on the selected installation's region, network path, downstream MTA capabilities, and deployment-profile guarantees.
