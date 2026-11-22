---
"ownsand-agent": patch
---

A snapshot import whose command context already ended before it took the per-snapshot lock now ends with `ABORTED` even when the lock is free. Before, the lock and the ended context were both ready, the lock could win, and the import could return a ready copy or continue to the download. The aborted import gives the lock back and leaves no lock entry or content behind.
