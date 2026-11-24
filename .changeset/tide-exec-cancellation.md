---
"tide-sdk": patch
---

Stop delivering buffered exec events after cancellation. Preserve an exec result if the collector received its terminal event before cancellation.
