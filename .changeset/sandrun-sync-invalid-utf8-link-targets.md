---
"sandrun-cli": patch
---

SandRun sync rejects symbolic link targets with invalid UTF-8 before it takes the sync lock on Windows guests. The error names the link so users can correct its target and sync again.

On Linux and macOS guests, sync preserves the original bytes of symbolic link targets with invalid UTF-8, including when the guest uses bsdtar.
