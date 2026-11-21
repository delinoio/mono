---
"ownsand-api": patch
---

End a snapshot or export attempt whose command could not be queued, for example because the request was canceled while the agent channel was full. `CreateSnapshot` now moves the record to `FAILED` and `ExportSnapshot` to `EXPORT_FAILED` with a database context that does not depend on the request, so a canceled request no longer leaves a snapshot in `CREATING` or an export in `EXPORTING` that blocks retries.
