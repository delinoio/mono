---
"accounts-api": patch
---

`accounts-api` now writes a `billing_audit_event` log record only after the transaction that inserted the audit row has committed. Before, a `CreateInvitation` call whose email could not be sent rolled back the invitation and its audit row but still logged an `invitation_created` record. Every other audited operation logs the same record, with the same fields, after its commit.
