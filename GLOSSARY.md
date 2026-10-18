# FRY

FRY supports business document exchange through email, with human review of exceptions.

## Language

**Organization**:
A business served by FRY whose domains, inboxes, and access permissions are managed independently and isolated from those of other organizations. Each incoming custom domain belongs to exactly one organization within a FRY installation. A business exchanging documents with an organization is not necessarily itself an organization in FRY.

**Organization manager**:
An internal Customer Satisfaction or Professional Services staff member who administers customer organizations within an authorized scope. Also called an organization administrator. Managers may administer only explicitly assigned organizations, potentially several per person; team membership alone does not grant access to all organizations. Assignments are granted or revoked only by global operators or their configured OIDC mappings; managers cannot delegate access.

**Global operator**:
A Technology or Development team member with deployment-wide FRY administration privileges across organizations. Also called a central FRY operator. This role does not imply access across independent FRY installations.

**Inbox**:
An organization-owned destination for incoming email. Multiple email addresses across that organization's domains can feed the same inbox; one configured address may also feed several distinct inboxes within that organization.

**Distribution alias**:
An incoming email address configured to deliver to several destination inboxes within the same organization. It is part of conventional recipient-address resolution and does not depend on optional application-routing rules. Overlapping addresses or aliases within one SMTP submission produce one copy per destination inbox, preserving relevant recipient-address metadata.

**Catch-all inbox**:
An explicitly configured inbox in a domain's owning organization that receives direct SMTP mail addressed to otherwise unconfigured addresses under that domain. Without a catch-all, such recipients are rejected. This is separate from a fallback application in optional application routing.

**Mailbox view**:
An optional, independent view of an inbox for one receiving application, with its own folders, read/unread state, and deletions. Changes in one application's view do not change another application's view. Conventional shared mailbox state is the default instead: clients accessing the same mailbox see each other's changes.

**Routing rule**:
An optional condition that selects receiving applications for an inbox message based on its sender, recipients, headers, or attachment types.

**Fallback application**:
The application designated for an inbox using application routing to receive messages that pass its enabled checks but are not selected for any application by its routing rules. Conventional mailbox operation does not require a fallback application.

**Delivery**:
A tracked handoff of a message to one receiving application. The same message can have several deliveries, each with independent progress.

**Delivery acknowledgment**:
A receiving application's confirmation that it has safely accepted a message. This confirms the handoff, not the outcome of the application's business processing.

**Delivery notification**:
A notice that a message is available for a receiving application to collect. Receipt of this notice does not confirm that the application has safely received the message itself.

**Business document**:
A document exchanged between companies or other business participants through email, such as an XML or PDF document.

**Exception**:
A case in the business document exchange that requires human review.

**Quarantine**:
A holding state for messages withheld from normal business delivery because of spam suspicion, a malware finding, or uninspectable content. Release follows the policy for the quarantine reason.

**Quarantine override**:
A privileged reviewer's decision to release a message despite a malware finding or an uninspectable password-protected ZIP, accompanied by a recorded reason. It preserves the original finding and does not reclassify the content as clean or inspected.

**Pending checks**:
A message state awaiting results of enabled required filtering checks before delivery to applications. Deliberately disabled checks do not create this state. It does not by itself indicate that the message is spam or contains malware.

**Retained original**:
A recoverable copy of an original email, including its attachments, kept for the organization's configured retention period independently of deletion from an inbox. Expiry of this recovery protection does not expire a message still present in a default shared mailbox, nor require a separate physical copy of its bytes. Permanent deletion does not restart this retention period.

**Retention grace period**:
A configurable extension beyond normal email retention for outstanding application acknowledgments, unavailable required filtering checks, or unreleased quarantined mail.
