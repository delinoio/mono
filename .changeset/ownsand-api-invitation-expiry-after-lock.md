---
"ownsand-api": patch
---

`AcceptInvitation` now checks the invitation expiry again after it locks the invitation row. Before, an invitation that expired while the acceptance waited for the row lock was still accepted, because the expiry check used a timestamp taken before the wait. Now such an acceptance fails with `not_found` and writes no membership, acceptance or audit event.
