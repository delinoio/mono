---
"ownsand-api": patch
---

Account for Hyper-V memory rounding during placement: a Windows device reserves an odd memory request rounded up to an even number of MiB, so the reservation cannot exceed the device memory limit.
