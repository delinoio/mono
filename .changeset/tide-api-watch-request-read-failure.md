---
"tide-api": patch
---

End a queue watch when reading its requests fails, before sending any events from the incomplete batch.
