---
"ownsand-agent": patch
---

Fix a data race in the agent update withdrawal that logged the target version after releasing the updater lock.
