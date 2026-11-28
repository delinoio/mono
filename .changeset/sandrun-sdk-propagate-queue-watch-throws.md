---
"sandrun-sdk": patch
---

Close the queue watch and propagate the exact value passed to `throw()`, even if the active stream fails to close.
