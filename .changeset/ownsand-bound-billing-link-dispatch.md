---
"ownsand-api": patch
---

Keep new billing link calls within their persisted dispatch window. An ownership lookup that finishes at or after the cutoff returns `unavailable` without calling Accounts. A lookup that finishes earlier leaves the full 15-second Accounts grace before reconciliation can start.
