---
"ownsand-sdk": patch
---

Closing a sandbox handle now cancels the peer session setup that is in flight, as the Go SDK does. The waiting operations fail with the new `OwnSandHandleClosedError` (`sandbox handle closed`), a session that opens after the close is closed and never installed, and a closed handle rejects later peer-to-peer operations. A relay-only handle keeps serving. `Sandbox.delete()` closes the handle only after `DeleteSandbox` succeeded, so a failed delete leaves the handle usable.
