---
"ownsand-api": patch
---

`ownsand-api` now closes the agent channel that owns an export when the application of its result fails with a permanent database error, but only while that channel is still the device's current channel. The permanent error is still not retried. Before, the export stayed `EXPORTING` and owned by the live channel, so neither a repeated `ExportSnapshot` nor a state report released it. The state report of the replacement channel now fails the abandoned attempt, and a later `ExportSnapshot` sends a new command.
