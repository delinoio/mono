---
"accounts-api": patch
---

Share a failed Logto Management API token refresh between the concurrent lookups that wait for it, so a burst of lookups sends one token request instead of one per caller.
