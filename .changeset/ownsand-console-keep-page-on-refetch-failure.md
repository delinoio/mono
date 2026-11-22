---
"ownsand-console": patch
---

A failed background refetch of loaded data no longer replaces the page, the organization navigation, open dialogs and typed values with a full-page error. The console keeps the loaded content and shows a "The last refresh failed" notice with "Try again" for transient errors, and the next successful fetch clears it. A failed first load and the terminal errors `not_found`, `permission_denied` and `unauthenticated` still show the full-page error.
