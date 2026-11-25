---
"ownsand-agent": patch
---

Retry failed helper status probes before Pause or Delete lists workers in the process test runtime. If the probes keep failing, Pause rolls back and Delete keeps the sandbox tracked for retry.
