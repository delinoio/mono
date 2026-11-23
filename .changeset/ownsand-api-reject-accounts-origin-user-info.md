---
"ownsand-api": patch
---

Reject an `ACCOUNTS_API_ORIGIN` that contains user information (`user@`, `user:password@` or an empty `@`) when the server loads its configuration. The error names `ACCOUNTS_API_ORIGIN` and does not include the value. An existing deployment that embeds credentials in `ACCOUNTS_API_ORIGIN` now fails to start until they are removed from the variable.
