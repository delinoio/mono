---
"tide-cli": patch
---

Tide sync rejects file names with invalid UTF-8 before it takes the sync lock. It also preserves guest files when an older sync manifest cannot establish which file to delete.
