---
"sandrun-sdk": patch
---

Increase the retry delay when a SandRun exec stream fails after replaying its start event. The first start and new output still reset the delay.
