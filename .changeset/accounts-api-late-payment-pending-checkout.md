---
"accounts-api": patch
---

Keep a pending subscription checkout pending when the verified payment of a different, previously canceled subscription arrives late. Only the payment of the open checkout transaction activates the new subscription; the late payment still adds its credit grant once and is logged as `billing_subscription_payment_ignored`.
