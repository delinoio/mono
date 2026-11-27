---
"ownsand-api": patch
---

Let callers retry a pending sandbox delete after an agent error. Reuse the original delete intent and share the outstanding command on the current agent channel.
