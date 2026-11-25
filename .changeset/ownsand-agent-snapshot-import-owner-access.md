---
"ownsand-agent": patch
---

Allow Unix agents to import snapshots when the process umask removes owner access from new directories. Imported files and explicitly archived directories keep their original permission bits.
