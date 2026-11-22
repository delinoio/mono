---
"accounts-api": patch
---

`AcceptInvitation` now reads the clock again inside the acceptance transaction, after the Logto profile lookup and the billing account lock wait, and checks the invitation state with that reading. Before, an invitation that expired while the request waited was still accepted, because the transaction reused the timestamp from before the lookup. The request now fails with `failed_precondition` and writes no membership, acceptance or audit row. The membership, acceptance and audit times use the acceptance reading.
