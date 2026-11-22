---
"ownsand-console": patch
---

Make the mock control plane treat an explicit sandbox device as an exclusive selection: an unknown device fails with `not_found` and an offline device with `unavailable`, instead of the sandbox running on another device.
