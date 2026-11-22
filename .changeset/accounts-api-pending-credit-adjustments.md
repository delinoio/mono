---
"accounts-api": patch
---

Apply an approved Paddle refund or chargeback that arrives before the payment of its purchase: the adjustment is stored as pending, and the webhook that creates the credit grant applies it in the same transaction, so the final balance no longer depends on the delivery order. Adds the `pending_credit_adjustments` table (migration `00004`) and the `pending` webhook result label.
