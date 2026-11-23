---
"accounts-api": patch
---

`CreateInvitation` now stores the `invitation_created` audit event in the same transaction as the invitation, before it sends the email. Before, the event was stored in a later transaction after the send, so a failing audit write after a recipient had already accepted the invitation returned `internal` for a committed membership and left no creation audit event. Now a failing audit write rolls back the invitation before any email is sent, and the server no longer revokes an invitation with `reason` `audit_failed`. A failed send still revokes the invitation with `reason` `delivery_failed` and returns `unavailable`; the audit history is then `invitation_created` followed by `invitation_revoked`.
