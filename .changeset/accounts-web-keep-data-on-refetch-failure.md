---
"delino-accounts": patch
---

A failed background refetch (a window-focus refetch, a poll, or the refetch after a change) no longer replaces the billing account page or one of its sections with an error. The loaded data, the selected tab, the open dialogs and the typed input stay, and a retryable error notice shows above them. A refetch of the account that fails with `NotFound` or `PermissionDenied` still replaces the page.
