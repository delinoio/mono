---
"ownsand-sdk": patch
---

Treat a local access token failure (an empty token or a throwing callback) as terminal instead of retrying it as a transport interruption.
