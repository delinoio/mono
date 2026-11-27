---
"sandrun-cli": patch
---

Fix sync when a file replaces a tracked directory with a case-only name change on Windows or a canonically equivalent name on macOS. Sync preserves guest-only files that block the replacement.
