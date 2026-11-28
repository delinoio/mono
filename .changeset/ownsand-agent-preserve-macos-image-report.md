---
"ownsand-agent": patch
---

Keep the macOS base image available for scheduling while the agent publishes a replacement. The agent continues to send heartbeats and reports the replacement after publication releases the image lock.
