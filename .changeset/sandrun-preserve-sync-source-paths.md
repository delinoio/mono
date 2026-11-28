---
"sandrun-cli": patch
---

Resolve relative sync sources in the filesystem on Unix. The CLI and MCP server now report missing path components and follow symbolic links before resolving `..`.
