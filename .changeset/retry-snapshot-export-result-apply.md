---
"ownsand-api": patch
---

Retry the application of a snapshot export result after a transient database error, and fail an export result that repeats a part number as an invalid part. A result that cannot be applied within the retry window closes the owning agent channel, so the replacement channel fails the abandoned export and a retry sends a new command.
