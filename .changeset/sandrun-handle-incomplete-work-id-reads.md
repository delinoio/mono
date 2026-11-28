---
"sandrun-api": patch
---

Wait for a complete list of IDs before dispatching organizations, expiring queued requests, or starting runner drivers. If the read fails, the scheduler logs the error and retries on a later scheduled pass.
