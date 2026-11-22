---
"ownsand-agent": patch
---

The guest helper on Windows now reads a file at a long absolute path (248 bytes or more, drive or UNC) for a file read. Before, the first check of the file used the path without the extended-length prefix and failed, although the open that follows it would have worked. The check now uses the same prefix as the open (`\\?\` for a drive path, `\\?\UNC\` for a UNC path), and still compares the identity, size and modification time from one handle, so a path swapped after the check is still detected.
