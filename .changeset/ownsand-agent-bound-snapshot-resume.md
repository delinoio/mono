---
"ownsand-agent": patch
---

Bound the Virtualization.framework snapshot resume-state wait. A stopped or failed VM releases the source command lock so the agent can reconcile its exit.
