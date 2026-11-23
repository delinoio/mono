---
"ownsand-api": patch
---

`UnlinkBillingAccount` now always confirms with delino-accounts that the organization has no billing link, also when `ownsand-api` has no cached link and no link attempt record, and fails with `unavailable` when delino-accounts cannot confirm it. `TransferOwnership` and `DeleteOrganization` now record a durable pending unlink by organization ID even when the former billing account is unknown, so a link that delino-accounts holds without a local trace is removed. Migration `0012_billing_pending_unlink_optional_account.sql` makes `billing_pending_unlinks.billing_account_id` nullable and keeps the existing rows. Before, such a link could not be removed by the owner, a transfer or a deletion.
