---
"ownsand-api": patch
---

Keep the instance lease through a failed renewal until the expire time of its last confirmed renewal, so that a transient database error no longer closes every agent channel. A refusal from the database still ends ownership at once.
