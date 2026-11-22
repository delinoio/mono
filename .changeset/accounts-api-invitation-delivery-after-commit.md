---
"accounts-api": patch
---

`CreateInvitation` now sends the invitation email after the invitation transaction has committed, not inside it. Before, a rolled-back transaction could already have sent an email for an invitation that did not exist. When the send fails, the server revokes the invitation (audit event `invitation_revoked` with `reason` `delivery_failed`, no `invitation_created` event) and the RPC returns `unavailable`, so the caller can invite the address again. The Resend idempotency key `invitation/<invitation_id>` still limits one invitation to one email.
