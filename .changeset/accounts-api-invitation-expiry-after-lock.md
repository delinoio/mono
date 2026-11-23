---
"accounts-api": patch
---

CreateInvitation now reads the clock after it holds the billing account lock, so an invitation for the same address that expired while the request waited for the lock no longer rejects the new invitation with already_exists. The new invitation's creation time, its expiry (the creation time plus the invitation lifetime), the audit event time, the response and the invitation email all use that post-lock time.
