---
"accounts-api": patch
---

Evict the cached Logto Management API token when a profile retry receives a 401, so the next lookup can use a fresh token.
