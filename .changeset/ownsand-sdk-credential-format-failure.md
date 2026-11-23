---
"ownsand-sdk": patch
---

End a call with a terminal credential error when an access token callback rejects or throws a value whose message cannot be read or formatted, instead of leaving the call pending with an unhandled rejection.
