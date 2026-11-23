---
"ownsand-api": patch
---

Start the 30-second relay acceptance timeout when the connection writer has sent the request frame to the agent, not when the frame entered the outbound queue. A request frame that waits behind a blocked writer now stays under the forwarding deadline and no longer ends with `unavailable` before the agent could see it.
