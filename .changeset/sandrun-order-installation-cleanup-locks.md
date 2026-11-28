---
"sandrun-api": patch
---

Prevent deadlocks between GitHub installation deactivation and runner profile deletion by locking requests and idle runners in the same order.
