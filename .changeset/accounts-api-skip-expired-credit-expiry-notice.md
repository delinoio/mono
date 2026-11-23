---
"accounts-api": patch
---

`accounts-api` no longer queues the "credit expires in 7 days" email for a credit grant that expires while the billing worker waits for the billing account lock. The worker now reads the time again after it holds the account lock and the grant row lock, and skips a grant whose expiry time has passed without setting its expiry notice time. A grant that still expires in the future keeps its single notice.
