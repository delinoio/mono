---
"ownsand-sdk": patch
---

Peer-to-peer setup now responds to cancellation while WebRTC is still processing `createOffer`, `setLocalDescription` or `setRemoteDescription`. Previously, closing the sandbox handle could leave callers without an abort signal waiting indefinitely and the pending peer connection open.

Each caller that aborts receives its own abort reason. Caller aborts cancel the shared setup only after the last waiter aborts. Closing the sandbox handle rejects every caller still waiting with `OwnSandHandleClosedError`, including callers without an abort signal. When either path cancels setup, the SDK closes the pending peer connection. A late WebRTC completion installs no peer.
