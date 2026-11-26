---
"sandrun-api": patch
---

Shut down SandRun API when its configured metrics listener fails permanently. Stop the lease workers and release the lease before draining public HTTP.
