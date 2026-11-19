---
"accounts-api": patch
---

Reject `ADDR` and `LOG_LEVEL` values that contain Unicode white space instead of trimming them, so a value such as a no-break space no longer selects the default or passes validation.
