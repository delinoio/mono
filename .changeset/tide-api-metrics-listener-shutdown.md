---
"tide-api": patch
---

Shut down Tide API when its configured metrics listener fails permanently. Stop the lease workers and release the lease before draining public HTTP.
