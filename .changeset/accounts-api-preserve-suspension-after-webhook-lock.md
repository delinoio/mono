---
"accounts-api": patch
---

Preserve billing suspension when a Paddle webhook waits for the account lock. A matched webhook now checks the suspension deadline after acquiring the lock, so it records any suspension that is due and preserves an existing suspension during continued nonpayment.
