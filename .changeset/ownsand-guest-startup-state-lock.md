---
"ownsand-agent": patch
---

The guest helper now owns its state directory before it deletes anything. It opens its listener first, then takes an exclusive lock on `guest.lock` in the state directory. Before, a second helper that failed to bind the listener of a running helper, or that used another listener, had already deleted the read snapshots of the running helper. Now the second helper exits with `state directory is in use` and leaves them alone, and a replacement of a stopped or crashed helper starts at once, because the operating system drops the lock when the process ends. The helper also waits for open streams, for at most 5 seconds, before it exits after a stop request, so an exec that ends in that time still reports its result.
