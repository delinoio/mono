---
"ownsand-api": patch
---

Keep the relay stream close frame when the agent relay connection's outbound queue is full. Each admitted stream reserves a place for its close frame, and the stream keeps its connection and grant slots until the writer has sent it.
