---
"ownsand-agent": patch
---

Preserve the last accepted chunk time when the guest helper restarts with a date outside the signed 64-bit Unix nanosecond range. Recovered writes keep their 15-minute retention, and the helper still reads existing write records.
