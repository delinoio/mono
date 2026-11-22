---
"ownsand-sdk": patch
---

Log the terminal outcome of a server-streaming RPC in the logger option: `ok` when the stream completes, the Connect code of the error that ends it, or `Canceled` when the consumer or caller ends it, instead of `ok` when the stream opens.
