---
"ownsand-api": patch
---

Reject agent state reports with invalid UUID v7 epochs before reconciliation. Close the channel if its first report is invalid, and continue acknowledging heartbeats if the channel has already reconciled a report.
