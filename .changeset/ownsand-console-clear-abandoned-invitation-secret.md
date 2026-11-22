---
"ownsand-console": patch
---

The console now deletes the pending invitation secret from `sessionStorage` when it loads at a route outside the invitation flow, when the Logto sign-in callback fails and when the user signs out. Before, only in-console navigation deleted it, so a later sign-in could return to an abandoned invitation.
