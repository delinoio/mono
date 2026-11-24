---
"ownsand-agent": patch
---

Retry a failed terminal record write after sandbox cleanup. Report `DELETED` only after the agent saves it, and reuse the completed runtime cleanup on retry.
