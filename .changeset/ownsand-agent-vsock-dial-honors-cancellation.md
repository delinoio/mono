---
"ownsand-agent": patch
---

The Firecracker runtime now stops a vsock dial to the guest helper as soon as the caller's context is canceled or its deadline passes. Before, the handshake wait ignored the context after the socket connected, so a canceled caller waited for the guest acknowledgement or the 10 second default, and a late acknowledgement returned a connection to a caller that had already given up. The dial now closes the pending connection and returns the context error.
