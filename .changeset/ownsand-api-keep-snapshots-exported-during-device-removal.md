---
"ownsand-api": patch
---

Lock the snapshot rows of a device before `RemoveDevice` reads their export state, so that a snapshot whose export finishes during the removal is kept without a local copy instead of being deleted with its R2 objects.
