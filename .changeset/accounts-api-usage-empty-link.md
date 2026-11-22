---
"accounts-api": patch
---

Ignore an empty organization link, whose unlink time equals its link time, when a usage report is attributed, so an empty link no longer takes a report from the older link that overlapped the window or hides that the report is unattributed.
