---
"ownsand-api": patch
---

Retry the control event of a sandbox delete when the sandbox lookup fails with an error other than a missing row. The event processor now stops the batch at that event, keeps the cursor at the event before it, and delivers the delete command to the connected agent on a later poll instead of skipping it.
