---
"ownsand-agent": patch
---

Enforce the five-second wait limit for a contended OwnSand guest helper Unix listener lock. A helper that misses the deadline leaves the socket path untouched.

Create macOS Unix listener lock files with owner read and write access even under a restrictive umask. Restarts reuse the same lock file.
