---
"ownsand-agent": patch
---

Limit the peer-to-peer data path of the agent to 256 sessions and 64 open request streams per session. An offer beyond the session limit gets a `resource_exhausted` answer without an SDP, so the SDK with the automatic transport uses the relay, and a request on a new stream beyond the stream limit gets one `resource_exhausted` error frame while the session stays open.
