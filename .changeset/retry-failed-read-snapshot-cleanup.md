---
"ownsand-agent": patch
---

Retry failed cleanup of OwnSand guest read snapshots so callers can reuse the read ID once the snapshot is removed.
