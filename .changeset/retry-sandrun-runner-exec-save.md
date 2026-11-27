---
"sandrun-api": patch
---

Retry saving the original runner exec ID after a database error. This avoids launching a second GitHub runner and allows SandRun to follow the original program through sandbox cleanup.
