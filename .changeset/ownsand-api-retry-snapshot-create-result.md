---
"ownsand-api": patch
---

`ownsand-api` now retries the database write of a create-snapshot result after a transient database error, for up to the export result retry window (15 seconds). A recovered retry ends in the state that the agent reported (`READY` or `FAILED`) without a second create command. When the window ends or a permanent error occurs, the server closes only the agent channel that owns the result, while it is still current, so the state report of the replacement channel reconciles the snapshot. The write now acts only on a `CREATING` snapshot, so a late or repeated result no longer overwrites a deleted or already settled snapshot. Before, one failed write left the snapshot `CREATING` for as long as the channel stayed connected.
