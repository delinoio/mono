---
"ownsand-api": patch
---

Cancel an in-progress lease acquisition callback when the lease workers stop, so the lease release and the loss callback do not wait for the control event cursor read.
