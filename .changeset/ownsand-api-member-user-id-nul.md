---
"ownsand-api": patch
---

`UpdateMemberRole`, `RemoveMember` and `TransferOwnership` now return `invalid_argument` for a `user_id` that contains a NUL byte, instead of an internal error from PostgreSQL.
