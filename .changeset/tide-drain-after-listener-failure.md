---
"tide-api": patch
---

Let active Tide API requests finish within the shutdown timeout after a permanent public listener failure. Close any connections still active at the deadline and report the original listener error.
