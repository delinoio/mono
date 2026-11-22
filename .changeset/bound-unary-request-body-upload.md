---
"ownsand-api": patch
---

End a unary request whose body is not complete 30 seconds after the request started with `deadline_exceeded`, so a client that stalls or sends one byte per second no longer holds a connection indefinitely. The agent channel, the relay connection and the relay RPCs are not affected.
