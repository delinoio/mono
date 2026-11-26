---
"sandrun-api": patch
---

Delete a dev session's sandbox when dispatch assigns its runner during membership removal. Keep session cleanup and API key revocation in the same transaction as membership removal.
