---
"ownsand-api": patch
"ownsand-agent": patch
---

Revoke a peer-to-peer session whose `OpenSession` the caller canceled or whose signaling wait timed out after the offer was sent. The control plane sends the revocation on the agent channel that signaled the session, with a bounded enqueue that does not depend on the canceled request context. The agent remembers a revocation of a session ID that it has not registered yet (at most 1024 session IDs, each for 1 hour) and answers the matching offer with `FAILED_PRECONDITION` instead of creating a peer connection.
