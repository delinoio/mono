---
"ownsand-agent": patch
---

Reject directory destinations when fetching the Firecracker guest kernel. If a directory appears before publication, the fetch fails, names the output path and cleans up staging without changing the directory or its contents.
