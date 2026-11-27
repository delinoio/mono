---
"ownsand-agent": patch
---

Keep refusing new sandbox creates after an agent restarts on a version that a signed manifest revoked. The agent continues to report the verified recovery target while it retries the manifest fetch.
