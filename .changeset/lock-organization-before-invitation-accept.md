---
"ownsand-api": patch
---

Stop `AcceptInvitation` from deadlocking with a concurrent `DeleteOrganization`. The RPC now locks the organization row (`FOR KEY SHARE`) before it locks the invitation row, so a deletion either waits for the acceptance to commit or makes it fail with `not_found` instead of failing one of them with an internal error.
