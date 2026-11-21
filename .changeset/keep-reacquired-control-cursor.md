---
"ownsand-api": patch
---

Keep the control event cursor that a new lease period stores. A batch of control events that was still in flight when the instance lost and reacquired the lease no longer overwrites the new cursor, and it delivers no further event.
