---
"accounts-api": patch
---

Reject a `user_id` that contains a NUL character with `invalid_argument` in `UpdateMemberRole`, `RemoveMember`, `TransferOwnership`, `LinkOrganization` and `ListManageableBillingAccounts`. Before, Postgres refused the value and the call failed with `internal` and an `rpc_internal_error` log record.
