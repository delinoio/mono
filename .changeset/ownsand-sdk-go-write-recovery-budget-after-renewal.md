---
"ownsand-sdk": patch
---

The Go SDK no longer starts a relay `WriteFile` retry after the 60-second recovery window of the unacknowledged chunk has ended. Before, a grant renewal that used up the window was still followed by a late `WriteFile` call. Now the write ends with a data path error that wraps the last recovery failure, unless the caller's context ended first. The initial write still starts normally, and a retry that starts in time may finish after the deadline with the same `write_id`.
