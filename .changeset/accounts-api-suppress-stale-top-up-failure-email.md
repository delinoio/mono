---
"accounts-api": patch
---

Stop sending the payment-failed email for a top-up that completed after its failure was queued. Before, a top-up that timed out and then completed through a late verified Paddle webhook still emailed the owners and admins that no credit was added, although the credit was applied. The worker now reads the top-up status on the first delivery and on every retry, abandons the notification and writes `billing_notification_suppressed` when the top-up has completed; failed top-ups still send the email.
