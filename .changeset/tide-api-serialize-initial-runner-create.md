---
"tide-api": patch
---

Apply the first sandbox creation result before the runner can retry so a duplicate rejection cannot leave a dev or agent session stuck starting.
