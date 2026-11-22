---
"delino-accounts": patch
---

Delete a pending billing account invitation token from `sessionStorage` when the app loads outside the invitation flow (an abandoned sign-in redirect or the reload after sign-out) and when the sign-in callback fails, so a later sign-in no longer lands on an old invitation.
