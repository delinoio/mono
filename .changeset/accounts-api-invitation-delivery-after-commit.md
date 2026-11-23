---
"accounts-api": patch
---

`CreateInvitation` now sends the invitation email after the invitation transaction has committed, not inside it. Before, a rolled-back transaction could already have sent an email for an invitation that did not exist. When the send fails, the RPC returns `unavailable` in every case, and the `invitation_created` audit event that was stored with the invitation stays in the audit history. The server then tries to revoke the undelivered invitation in a separate transaction:

- If the revocation succeeds, the audit event `invitation_revoked` with `reason` `delivery_failed` follows `invitation_created`, and the caller can invite the address again.
- If the revocation fails, the server logs `invitation_revoke_failed` and the invitation stays pending. Another `CreateInvitation` for the same address returns `already_exists` until the invitation expires, so an owner or admin must call `RevokeInvitation` for it before the caller invites the address again.

The Resend idempotency key `invitation/<invitation_id>` still limits one invitation to one email.
