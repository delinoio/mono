---
"ownsand-sdk": patch
---

End a peer-to-peer operation that is in flight when the sandbox handle closes with `OwnSandHandleClosedError` instead of moving the handle to the relay, so a closed handle keeps rejecting later operations and keeps its `PEER_TO_PEER` transport.
