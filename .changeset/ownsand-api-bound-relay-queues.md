---
"ownsand-api": patch
---

Limit the number of queued frames per relay stream. When an incoming frame would exceed the limit, close the affected stream. Accepted calls can resume from their last received offsets.
