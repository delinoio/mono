---
"accounts-api": patch
---

`UpdateAutoTopUpSettings` now returns the settings that the request saved. The response, including the month-to-date total, is read in the update transaction before it commits. A concurrent update can no longer replace the returned settings, and a read failure now rolls back the settings change and its audit event instead of reporting a saved change as failed.
