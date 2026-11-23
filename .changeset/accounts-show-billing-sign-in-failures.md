---
"delino-accounts": patch
---

The billing sign-in view now shows an alert when sign-in fails before the redirect starts, for example when the Logto tenant is unreachable. Before, "Sign in with Delino" showed nothing. The button stays usable and the next attempt clears the alert. A build without a Logto registration still shows that sign-in is not configured.
