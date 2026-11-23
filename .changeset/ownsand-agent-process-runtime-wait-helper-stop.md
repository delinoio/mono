---
"ownsand-agent": patch
---

The process test runtime now waits until the guest helper is observed stopped before it enumerates the exec workers on pause and delete. A helper that does not stop in time makes pause roll back and delete return an error, so a worker that the helper starts after the enumeration is no longer left running or orphaned.
