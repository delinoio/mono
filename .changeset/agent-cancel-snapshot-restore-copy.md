---
"ownsand-agent": patch
---

A process-runtime sandbox restore that is canceled while it copies the snapshot disk now stops promptly, removes the partial sandbox directory and starts no guest helper. Before, the copy of a large snapshot ran to the end and the helper started although the restore was canceled.
