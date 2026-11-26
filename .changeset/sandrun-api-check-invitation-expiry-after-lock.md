---
"sandrun-api": patch
---

Reject invitations that expire while acceptance waits for a database lock. Use the time of the expiry check for the membership join and invitation acceptance timestamps.
