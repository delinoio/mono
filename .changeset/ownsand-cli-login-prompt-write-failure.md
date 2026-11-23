---
"ownsand-cli": patch
---

`ownsand login` now exits with status 1 and the write error when it cannot write the sign-in instructions (the verification URL and the user code) to standard error, and it sends no token request in that case. Before, it ignored the write error and polled until the device code expired, so a user who never saw the instructions waited for a sign-in that could not finish.
