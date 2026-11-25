---
"tide-sdk": patch
---

Increase the retry delay when a Tide exec stream fails after replaying its start event. The first start and new output still reset the delay.
