---
"ownsand-console": patch
---

The sign-in callback route no longer renders an empty page when no Logto sign-in is in progress: it now replaces the address with the organization list, or with the invitation page when a pending invitation secret exists. A real sign-in redirect still completes first and navigates after the code exchange.
