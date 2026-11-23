---
"ownsand-console": patch
---

The console now shows an alert when sign-in fails before the redirect starts, for example when the Logto tenant is unreachable. It appears on the signed-out screen, next to "Sign in to accept" and next to the "Sign in with Delino" button of an ended-session notice. Before, these showed nothing. The button stays usable, the pending invitation secret is kept for the retry, and the next attempt clears the alert.
