---
"ownsand-api": patch
---

Retry due pending billing unlinks at every 10-second step of the status worker on the lease holder instead of only at the 1-minute status poll, so an unlink that `accounts-api` could not confirm is retried at its back-off time.
