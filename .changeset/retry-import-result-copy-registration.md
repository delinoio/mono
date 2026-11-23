---
"ownsand-api": patch
---

The control plane now retries the write of the copy row for a successful snapshot import result after a transient database error, so the importing device stays in the fanout of a later snapshot deletion. When the write cannot be applied, the server closes the channel that is still current, and the state report of the replacement channel recovers the copy.
