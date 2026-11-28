---
"ownsand-cli": patch
"sandrun-cli": patch
---

Reject Logto resource token lifetimes that would overflow during login or refresh. Report a fixed `expires_in` error and preserve any existing credentials.
