---
"ownsand-sdk": patch
---

Reject `createSandbox` with a configuration error before it sends `CreateSandbox` when the transport is forced to `PEER_TO_PEER` and no WebRTC implementation is available, so no billable sandbox starts that the client cannot open.
