---
"ownsand-api": patch
---

Delete a control event only after the lease holder has processed it. The janitor ran the control event cleanup on every instance by age alone, so an event that the lease holder had not processed yet, for example a `delete_snapshot` delivery during a long database stall, could expire after 1 hour and never reach the device. The cleanup now runs only on the lease holder and deletes only events up to its in-memory processed cursor that are older than 1 hour.
