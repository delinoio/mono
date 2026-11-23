---
"ownsand-api": patch
---

Limit every request message to 2 MiB after decompression. A unary request whose body exceeds the limit ends with `resource_exhausted` before the authentication interceptor, a `RelayService` request authenticates at its request gate before the body is read, and a streaming RPC ends with `resource_exhausted` when it reads an oversized message, which includes an agent channel. A compressed message within the limit is processed normally.
