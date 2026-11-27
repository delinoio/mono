---
"accounts-api": patch
---

Expire paid credit 12 UTC calendar months after an unambiguous Paddle payment capture. When a delayed webhook creates a grant whose expiry has passed, record the purchase and expire its unused credit in the same transaction.
