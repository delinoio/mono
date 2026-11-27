---
"sandrun-cli": patch
---

Reject SandRun directory downloads when distinct regular file paths resolve to one local entry because of case or Windows path separators. Also reject case collisions between a regular file and a symbolic link before replacing the destination.
