---
"ownsand-api": patch
---

`ownsand-api` now retries the database update that ends an unsent or rejected create (a sandbox create, a failed import, an expired import wait, or `CreateSnapshot`) after a transient database error, for up to `Timing.CreateFailTimeout` (5 seconds). When a permanent error or the end of the budget leaves the record `CREATING`, the server closes the agent channel that was meant to receive the command, but only while it is still the device's current channel, so the state report of the replacement channel settles the record. Before, a single failed update left the reservation held for as long as the channel stayed connected.
