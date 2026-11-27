---
"sandrun-cli": patch
---

Fix macOS workspace sync retries after a file becomes a directory whose name differs only in case. A retry restores synced files and preserves guest-created files in the directory.
