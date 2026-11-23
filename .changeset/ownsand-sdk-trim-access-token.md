---
"ownsand-sdk": patch
---

An access token with leading or trailing whitespace now sends the same `Authorization` header as the bare token. Before, leading whitespace stayed in the header value, and a leading line break made the request fail. A token with a line break or NUL inside it still ends the call with a terminal credential error.
