---
"delino-accounts": patch
---

Refresh a pending subscription on the billing Overview without a reload. While the subscription is `PENDING` and the tab is in the foreground, the page reads it every 2 seconds. When it leaves `PENDING`, the page reads the balance and the credit grants again, so the Top up action and the new credit appear as soon as Paddle confirms the payment.
