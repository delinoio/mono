---
"accounts-api": patch
---

Keep observing a connection after a request whose parsed `Connection` close flag is set, because `net/http` can still reuse the connection (for example an HTTP/1.0 request with `Connection: keep-alive` followed by `Connection: close`). A later `417 Expectation Failed` on that connection now gets its `http_request` log record.
