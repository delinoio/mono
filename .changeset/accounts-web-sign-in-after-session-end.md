---
"delino-accounts": patch
---

A billing request that fails with `Unauthenticated`, for example after the Logto access token could not be refreshed, now shows a "Sign in with Delino" button in place of "Try again". Before, the user could only retry a request that could not succeed. On the invitation page, the button also saves the invitation token again so that the sign-in redirect returns to it.
