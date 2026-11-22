---
"ownsand-api": patch
---

Decide the instance lease takeover and write its `expire_time` with the database clock only, so that a wall-clock step of one instance cannot let it take a valid lease or delay its takeover of an expired one.
