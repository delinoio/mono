---
"ownsand-agent": patch
---

The agent now measures the idle time of a forwarded UDP flow with the monotonic clock. Before, the shared activity time was a wall-clock timestamp, so a forward or backward step of the system clock could close a live flow early or keep an idle flow open past its 90 second limit. A datagram in either direction still moves the shared deadline, and a flow without datagrams still closes after the idle time.
