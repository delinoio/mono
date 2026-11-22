---
"ownsand-sdk": patch
---

The peer-to-peer setup now handles a `RTCPeerConnection` constructor that throws. Before, the raw error ended the operation, so an automatic transport did not fall back to the relay and a forced `PEER_TO_PEER` transport did not return the data path error. The setup now treats the throw like any other peer setup failure: the handle moves to the relay, or a forced `PEER_TO_PEER` transport returns a `DataPathError` whose cause chain holds the original error. A caller abort still ends the operation with the abort reason.
