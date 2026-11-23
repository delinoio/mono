---
"ownsand-agent": patch
---

The guest helper no longer extends the lifetime of a staged write when a chunk fails. Before, a rejected chunk (for example, one that overlapped written bytes with other content) refreshed the live expiry time, so the helper kept the staging file longer than a restarted helper would. Both the live time and the recovery record now keep the time of the last accepted chunk.
