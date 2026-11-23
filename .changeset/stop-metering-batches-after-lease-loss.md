---
"ownsand-api": patch
---

A usage reporting pass now checks the instance lease immediately before each batch RPC. An instance that lost the lease during a pass sends no later batch and leaves the unsent outbox rows due and unchanged for the new lease holder; a batch RPC that already started finishes as before.
