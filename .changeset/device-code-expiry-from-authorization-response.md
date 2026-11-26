---
"ownsand-cli": patch
"sandrun-cli": patch
---

Count the device code lifetime of `login` from the device authorization response, so a login whose sign-in prompt write blocks past the lifetime ends with the expiry error without sending a token request.
