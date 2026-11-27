---
"ownsand-agent": patch
---

Limit each sandbox to 32 held execs. At capacity, a new exec evicts the oldest ended result or returns a retryable capacity error if all held execs are running.
