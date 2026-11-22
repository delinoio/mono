---
"ownsand-api": patch
---

`LinkBillingAccount` now records a durable link attempt before it calls `delino-accounts` and records the link in a second transaction. Before, it called `delino-accounts` inside the transaction that records the link, so a lost response or a failed local commit could leave an active billing link without any local trace. An unconfirmed attempt is now removed by an explicit unlink, an ownership transfer, an organization deletion or the status worker, and an authorized retry of the same billing account links again. A rejected link leaves no record and sends no unlink. Migration `0010_billing_link_attempts.sql` adds the nullable columns `link_attempt_id` and `link_call_deadline` to `billing_pending_unlinks`.
