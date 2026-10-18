# Deliver complete emails to selected applications

When optional application routing is enabled, attachment-type rules select which applications receive an email, and each selected application receives the complete email with all its attachments available. For example, an XML rule selecting an application for an email containing XML and PDF makes both attachments available to that application; routing does not produce an attachment-only subset.

When ZIP inspection is enabled, it identifies document types for filtering or routing while delivery preserves the complete email and original ZIP attachment. Under that inspection policy, password-protected ZIPs quarantine the whole email as uninspectable in the first release; designated organization security reviewers and central FRY operators may release it with a recorded reason while preserving the uninspectable finding. Extraction limits remain to be defined.

Milestone scope: these optional capabilities are deferred until after the first production milestone proves conventional email operation. See [milestone sequencing](0014-prove-conventional-email-first.md).
