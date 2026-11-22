---
"ownsand-agent": patch
---

Stop a compute system that a failed earlier attempt left for the sandbox before the Hyper-V runtime removes the VM directory on create, so the leftover VM no longer holds its disk open and delays the removal with a sharing violation.
