---
"ownsand-cli": patch
---

Preserve the destination file's group when a download replaces it on Linux or macOS. If the CLI cannot set the group, it leaves the original file in place and removes the temporary file.
