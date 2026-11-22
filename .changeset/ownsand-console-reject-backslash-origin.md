---
"ownsand-console": patch
---

Reject a backslash in `PUBLIC_API_ORIGIN` and `PUBLIC_LOGTO_ENDPOINT`, because the URL parser reads a backslash as a path separator and would otherwise accept a hidden path as a bare origin.
