---
"ownsand-api": patch
---

Serialize concurrent device removals and a removal that races an organization deletion, so each removal writes one audit event and one control event and keeps its first tombstone timestamps.
