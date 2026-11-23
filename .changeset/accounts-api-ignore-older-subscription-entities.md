---
"accounts-api": patch
---

Ignore a Paddle subscription entity that is older than the stored one also when it belongs to a different subscription, so that a late event of an earlier subscription no longer replaces a canceled later subscription and makes `StartSubscription` fail with `already_exists`.
