---
"accounts-api": patch
---

`UnlinkOrganization` now reads the clock for the unlink time and its audit event after it holds the billing account lock, instead of at the start of the request. A usage window that starts while the unlink waits for the lock is still attributed to the billing account.
