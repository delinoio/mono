---
"ownsand-sdk": patch
---

The peer-to-peer session now validates a session-end frame before it ends the session, as the Go SDK does. A session-end frame on stream 0 or without its payload no longer ends the whole session: it fails only its open stream, or is ignored when no stream is open.
