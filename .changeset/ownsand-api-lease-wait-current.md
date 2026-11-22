---
"ownsand-api": patch
---

The lease wait now returns only while the instance holds a current lease. Before, it could report readiness for an acquisition whose expiry had passed but whose loss the watcher had not recorded yet, because it returned on the closed acquisition channel.
