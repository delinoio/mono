---
"ownsand-console": patch
---

Each opening of a form dialog now has its own submission state. After a dismissal, a new opening starts without a pending or error state, and a late result of an earlier request no longer shows an error in it, closes it, opens a secret, token or link dialog, navigates away, or clears the pending state of the new request. The lists still refresh after such a request succeeds.
