---
"ownsand-agent": patch
---

Linux guest init closes the helper's process descriptor before the restart delay. Repeated helper restarts no longer leave descriptors open until garbage collection.
