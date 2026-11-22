---
"ownsand-api": patch
---

Retry the update that ends an `ExportSnapshot` attempt whose command could not be queued after a transient database error, and close the owning agent channel when the update stays unresolved. A database fault during the send failure no longer leaves the snapshot `EXPORTING` on a live channel that nothing releases.
