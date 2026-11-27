---
"accounts-api": patch
---

Close active public HTTP connections when the shutdown drain expires, before the server waits for background cleanup.
