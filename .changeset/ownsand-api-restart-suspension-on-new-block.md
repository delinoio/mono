---
"ownsand-api": patch
---

Restart the 3-day suspension period when an organization linked to a billing account recovers and is blocked again before the control plane polls, so that the new non-payment interval does not inherit the earlier block start. The control plane now stores the block start that `delino-accounts` last reported for the link (the new nullable column `organization_billing.remote_blocked_since`, added by migration `0016_billing_remote_blocked_since.sql`) and treats a later reported block start as a new interval. Continuing non-payment, links, unlink-then-link, retries and repeated polls still keep the earlier start, and the 3-day grace period is unchanged.
