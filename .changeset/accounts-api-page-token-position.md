---
"accounts-api": patch
---

Reject a page token with a negative time or a NUL character in its position with `invalid_argument` in `ListMembers`, `ListInvoices`, `ListLinkedOrganizations` and `ListUsageSummary`. Before, a forged token with a NUL character failed with `internal` and an `rpc_internal_error` log record, and a negative time was accepted.
