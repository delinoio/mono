---
"ownsand-api": patch
---

Check the instance lease every 10 seconds in the usage reporter of an instance that does not hold it, so a new lease holder reports the due outbox rows within about one tick instead of after the 5-minute idle interval when no new window was sealed.
