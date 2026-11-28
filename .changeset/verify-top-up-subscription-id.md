---
"accounts-api": patch
---

Keep a Paddle top-up outcome unknown when the charge response omits the subscription ID or returns a different ID. A retry with the same key continues to use the recorded outcome.
