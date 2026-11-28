---
"ownsand-api": patch
---

For Hyper-V placement on Windows devices, the control plane adds 1 MiB to odd memory requests below 9223372036854775807 MiB. A request of 9223372036854775807 MiB reserves exactly that amount. Placement checks the resulting reservation against the device memory limit.
