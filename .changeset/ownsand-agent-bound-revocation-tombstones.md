---
"ownsand-agent": patch
---

Remove revocation tombstones from storage as soon as the agent consumes them. The agent retains at most 1,024 tombstones, including when offers arrive out of order.
