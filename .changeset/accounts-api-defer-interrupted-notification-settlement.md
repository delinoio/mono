---
"accounts-api": patch
---

Preserve billing notification retries when the delivery pass ends while the worker records the final recipient. The worker keeps the recorded delivery and retries pending recipients after one minute.
