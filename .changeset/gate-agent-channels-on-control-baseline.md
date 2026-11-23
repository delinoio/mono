---
"ownsand-api": patch
---

Refuse agent channels until the control event baseline of the lease period is stored, so a sandbox delete or device limits change that commits while the baseline is being established still reaches the connected agent.
