---
"ownsand-console": patch
---

The callback page now shows the sign-in error notice when the stored Logto sign-in session cannot be read (text that is not JSON, or an object without a string `redirectUri`, `codeVerifier` and `state`). It stays on `/callback` and removes the pending invitation secret. Before, the page stayed blank, kept the secret and raised an unhandled rejection from the Logto SDK.
