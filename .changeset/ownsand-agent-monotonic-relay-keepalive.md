---
"ownsand-agent": patch
---

The relay keepalive of the agent now counts the 30-second interval on the monotonic clock. Before, it compared wall-clock times, so a clock step could delay the keepalive until the 90-second receive timeout of the control plane closed the connection, or send keepalives too early. A successful send still resets the interval, and a full outbound queue still skips the keepalive without waiting.
