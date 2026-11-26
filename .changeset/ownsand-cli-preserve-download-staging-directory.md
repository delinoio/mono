---
"ownsand-cli": patch
---

Preserve the destination directory path when staging file downloads. On Linux and macOS, a missing directory before `..` now produces a local error before the CLI requests the file.
