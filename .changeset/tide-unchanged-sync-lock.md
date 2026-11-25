---
"tide-cli": patch
---

Check the guest sync lock before reporting that a Tide CLI or MCP sync has no changes. A forced sync can clear a stale lock even when the files match.
