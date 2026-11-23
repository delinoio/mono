---
"ownsand-console": patch
---

Make the mock API reject `AcceptInvitation` for an invitation at or after its expiry time with `NotFound`, like an unknown secret and before the already-a-member check, and leave the invitation pending and the memberships unchanged.
