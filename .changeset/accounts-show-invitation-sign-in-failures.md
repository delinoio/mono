---
"delino-accounts": patch
---

The invitation page now shows an alert when sign-in fails before the redirect starts, for example when the Logto tenant is unreachable. Before, "Sign in to accept" and the sign-in button of an ended session showed nothing. The buttons stay usable, the pending invitation is kept for the retry, and the next attempt clears the alert.
