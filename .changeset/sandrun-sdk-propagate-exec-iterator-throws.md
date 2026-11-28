---
"sandrun-sdk": patch
---

Close the exec iterator and propagate the exact value passed to `throw()`, even if the active stream fails to close.
