---
"accounts-api": patch
---

Keep delivering a billing notification to the remaining owners and admins after one recipient fails. Before, a recipient whose profile read or email send kept failing stopped the delivery at that recipient, so later recipients never received the email before the notification was abandoned. The `billing_notification_failed` record now also carries `failed_recipients`.
