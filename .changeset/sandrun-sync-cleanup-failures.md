---
"sandrun-cli": patch
---

Report temporary file removal failures after `sandrun session sync` and MCP sync on Linux and macOS guests. Cleanup still attempts to remove the remaining files and release the sync lock.
