---
"sandrun-cli": patch
---

Allow `sandrun session sync` and the MCP sync tool to retry a file-to-directory change on Linux and macOS after the previous sync applied the change but failed to write its manifest. The retry preserves files created by the guest in the new directory.
