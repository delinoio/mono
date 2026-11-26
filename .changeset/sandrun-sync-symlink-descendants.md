---
"sandrun-cli": patch
---

When an unstaged symbolic link replaces a directory, SandRun sync deletes the previously synced files under the old directory. It preserves the replacement as a link when Git selects it.
