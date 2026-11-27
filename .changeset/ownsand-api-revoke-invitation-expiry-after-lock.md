---
"ownsand-api": patch
---

Reject `RevokeInvitation` with `failed_precondition` if the invitation expires while the request waits for its database lock. Leave the revocation timestamp and audit history untouched. Successful revocations store the time used for the expiry check.
