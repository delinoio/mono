---
"ownsand-api": patch
---

Wait for the billed requests that are still running before the final usage flush at shutdown, close the usage recorder with that flush, and report an incomplete drain.
