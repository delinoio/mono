---
"ownsand-sdk": patch
---

Go SDK: a `canceled` status that the control plane sends for `GetIceServers` or `OpenSession` while the caller's context is live now moves an automatic handle to the relay, like any other setup failure, and a forced peer-to-peer handle returns it without an `AuthorizeRelay` call. A cancellation by the caller still ends the operation with the context error.
