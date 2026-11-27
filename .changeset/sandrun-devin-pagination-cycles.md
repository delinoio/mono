---
"sandrun-api": patch
---

Stop Devin pagination when a continuation repeats. Start the queue watch from the last advancing cursor and retry unmatched outpost lookups after backoff.
