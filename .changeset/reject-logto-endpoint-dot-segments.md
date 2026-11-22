---
"delino-accounts": patch
---

Reject a `PUBLIC_LOGTO_ENDPOINT` whose path the URL parser collapses to `/`, such as `/tenant/..`, `/%2e` or a backslash path, so the build fails instead of silently using the origin of a value that was not a bare origin.
