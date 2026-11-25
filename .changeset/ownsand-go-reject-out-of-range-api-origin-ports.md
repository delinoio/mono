---
"ownsand-sdk": patch
---

The Go SDK rejects API origins with explicit ports above 65535 when it constructs the client. The error omits the configured URL.
