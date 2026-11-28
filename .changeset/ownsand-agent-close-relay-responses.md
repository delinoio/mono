---
"ownsand-agent": patch
---

Close relay HTTP response bodies when their requests end so unread data can release HTTP/2 flow-control credit on the shared connection. Response cleanup runs independently of reconnect.
