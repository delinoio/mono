---
"ownsand-agent": patch
---

The agent now resends the unacknowledged terminal update of an ended sandbox every 10 seconds until the control plane acknowledges it. Before, the update of a sandbox that ended by a delete command, a TTL expiry or a lost VM was sent once and then skipped until the next channel reconnect or agent restart. A sandbox whose end is still running is still not resent as ended.
