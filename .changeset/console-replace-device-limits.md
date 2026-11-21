---
"ownsand-console": patch
---

Save the device limits with the strict ReplaceDeviceLimits RPC, so an explicit zero is stored as zero and a limit that the form does not have is never sent as a silent zero.
