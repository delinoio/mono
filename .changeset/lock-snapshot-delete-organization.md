---
"ownsand-api": patch
---

Serialize snapshot deletion with organization deletion before locking the snapshot row, preventing a deadlock when an imported copy is being removed.
