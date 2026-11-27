---
"ownsand-agent": patch
---

On Linux, flush the state directory and every ancestor before the first enrollment request, even when another process created the directories. If a required flush fails, keep the pending key for retry and leave the enrollment token unused.
