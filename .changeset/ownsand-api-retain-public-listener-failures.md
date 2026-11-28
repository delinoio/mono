---
"ownsand-api": patch
---

OwnSand API now reports a permanent public listener failure that overlaps a termination signal. The process logs the listener failure and any cleanup or final usage errors, then exits with status 1.
