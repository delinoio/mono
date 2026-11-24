---
"ownsand-api": patch
---

Keep Windows sandbox memory allocations within the signed 64-bit range so device reads, limit updates, and sandbox placement can use a reservation of 9223372036854775807 MiB.
