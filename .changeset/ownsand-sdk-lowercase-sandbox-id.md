---
"ownsand-sdk": patch
---

Store the sandbox ID of a handle in lowercase when it is a UUID v7 in either hex case, so relay request digests, relay requests and peer-to-peer frames use the canonical spelling.
