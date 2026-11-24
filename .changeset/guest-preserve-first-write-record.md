---
"ownsand-agent": patch
---

Keep the guest helper's first write record when startup cannot rename it. The helper can still expire the unfinished write and remove its staging file if the file identity matches. A later restart can retry the rename.
