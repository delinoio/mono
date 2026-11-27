---
"ownsand-agent": patch
---

Keep the process test runtime's sandbox and socket files when restart cleanup cannot read the socket directory record. The agent retries cleanup before reporting the sandbox as ended.
