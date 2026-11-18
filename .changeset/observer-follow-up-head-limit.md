---
"accounts-api": patch
---

Keep logging the requests that follow a large request head on a kept-alive connection, such as a `417 Expectation Failed` rejection or a header read timeout.
