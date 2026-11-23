---
"ownsand-api": patch
---

The 2-minute retention window of unanswered agent commands is now measured in elapsed (monotonic) time. A forward or backward wall-clock correction no longer expires a record early or keeps an expired one, and the result handling and the sweep use the same window. Persisted timestamps and lease timing are unchanged.
