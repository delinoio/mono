---
"accounts-api": patch
---

RevokeInvitation now reads the clock after it holds the billing account and invitation locks. An invitation that expires while the revocation waits for a lock is rejected with `failed_precondition`, keeps a NULL `revoke_time` and writes no `invitation_revoked` audit event. A successful revocation uses that one time for the stored `revoke_time`, the audit event and the response.
