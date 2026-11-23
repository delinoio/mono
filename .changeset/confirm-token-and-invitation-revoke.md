---
"ownsand-console": patch
---

Revoking a pending enrollment token or a pending invitation now opens a confirmation dialog first, like revoking an API key. Cancel comes first and has the focus, and no request is sent until the user confirms. A failed revoke keeps the dialog open with its error notice, and a second confirmation retries it; the failure is no longer shown as a toast. The success toasts are unchanged.
