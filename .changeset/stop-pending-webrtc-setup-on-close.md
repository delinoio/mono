---
"ownsand-sdk": patch
---

Closing a sandbox handle, or the abort of every caller that waits for the peer-to-peer setup, now ends the setup while the WebRTC stack has not yet answered `createOffer`, `setLocalDescription` or `setRemoteDescription`. Before, a caller without an abort signal waited for the setup without an end, and the pending peer connection stayed open. The waiting callers now fail with `OwnSandHandleClosedError` and the peer connection closes. A late completion installs no peer.
