---
"ownsand-api": patch
---

Stop a relay stream from queuing ordinary frames after it closed: a request frame or window update that waited for room in the agent connection's outbound queue while its stream closed is now rejected instead of being queued behind the stream close frame, so it no longer wastes queue capacity and relay bandwidth.
