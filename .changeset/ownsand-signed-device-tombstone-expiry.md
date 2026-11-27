---
"ownsand-api": patch
---

Recheck the device removal deadline after locking the row for Authenticate and ConfirmRemoval. Return `not_found` at or after expiry and leave the nonce and tombstone untouched.
