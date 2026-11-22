---
"ownsand-api": patch
---

Guard the lazy initialization of the control event cursor with the cursor generation. A `LatestID` read that the event processor started before a lease loss or reacquisition no longer replaces or clears the cursor that the later lease transition stored, so the lease holder does not replay control events from before its lease period.
