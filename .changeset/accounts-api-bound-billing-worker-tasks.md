---
"accounts-api": patch
---

Apply the automatic top-up claim budget to database lock waits so a blocked claim leaves the top-up pending. Limit notification delivery in each periodic pass so the worker has time to refresh linked-organization metrics even when due notices remain.
