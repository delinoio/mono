---
"ownsand-sdk": patch
---

Stop starting a WriteFile retry after the 60 second chunk recovery window ran out and return the last failure instead.
