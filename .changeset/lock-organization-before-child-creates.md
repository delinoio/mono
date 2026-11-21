---
"ownsand-api": patch
---

Stop `CreateSandbox`, `CreateSnapshot` and `CreateTemplate` from deadlocking with a concurrent `DeleteOrganization`. Each RPC now locks the organization row (`FOR KEY SHARE`) before it locks any device, sandbox or snapshot row, so a deletion either waits for the create to commit or makes it fail with `not_found` instead of failing one of them with an internal error.
