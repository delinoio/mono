---
"sandrun-cli": patch
---

Report a missing guest exit status as an error. Sync stops before it uploads files when lock acquisition lacks a status, and skips the manifest write when apply lacks a status.
