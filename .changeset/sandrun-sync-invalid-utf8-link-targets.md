---
"sandrun-cli": patch
---

SandRun sync rejects symbolic link targets with invalid UTF-8 before it takes the sync lock on Windows guests. The error names the link so users can correct its target and sync again.
