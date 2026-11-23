---
"ownsand-api": patch
---

A late `unauthenticated` answer from `accounts-api` no longer drops a newer machine-to-machine access token. The billing token source now invalidates only the token that the rejected call sent, so a replacement token, or a replacement request in progress, survives a late rejection of the old token and the next usage report does not depend on the token endpoint. Token lifetime, issuer behavior and the billing retry policy do not change.
