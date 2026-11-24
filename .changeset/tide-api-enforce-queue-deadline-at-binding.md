---
"tide-api": patch
---

Expire queued dev and agent sessions that reach their queue deadline before a runner starts. Keep idle warm runners available, delete unused cold sandboxes, and release vendor claims after expiry.
