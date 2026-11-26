---
"ownsand-sdk": patch
"ownsand-cli": patch
---

Start the Go SDK's peer-to-peer file write attempt deadline before sending the request. The send and result wait share one deadline, including time spent waiting for the data channel to accept more data.
