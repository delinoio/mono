---
"ownsand-agent": patch
"ownsand-api": patch
---

Creating a sandbox with a `disk_gib` above 8589934591 now fails with `invalid_argument` that names `resources.disk_gib`, on the control plane before scheduling and on the device agent before its capacity check, because the byte count of a larger disk overflows a signed 64-bit integer.
