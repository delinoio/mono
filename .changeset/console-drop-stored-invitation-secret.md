---
"ownsand-console": patch
---

Drop the invitation secret that an earlier link left in `sessionStorage` when a new invitation link opens, so a later load without a fragment no longer accepts the earlier invitation.
