---
"ownsand-api": patch
---

The agent relay connection now measures its idle close and keepalive thresholds with elapsed monotonic time. A forward or backward step of the wall clock no longer closes a healthy connection early, keeps a silent one open, or moves a keepalive. `LastReceive` and `LastLargeReceive` still report wall timestamps.
