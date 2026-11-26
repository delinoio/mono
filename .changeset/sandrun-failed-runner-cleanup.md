---
"sandrun-api": patch
---

Cancel stalled cleanup when SandRun loses its lease and apply a deadline to each attempt. Retry GitHub registration removal after a failed runner start before deleting the sandbox.
