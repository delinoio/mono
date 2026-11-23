---
"ownsand-api": patch
---

Report the exact vCPU figures of a device at full precision. `DeviceAllocation.vcpu_total_count` (`int64`) carries the exact allocated vCPUs and `Device.effective_vcpu_limit_count` (`int64`) carries the exact effective vCPU limit. The `int32` fields `vcpu_count` and `effective_vcpu_limit` stay for v1 compatibility and are capped at 2147483647 for a larger value. `GetDevice` no longer fails with an internal error when the allocation exceeds the `int32` range, and placement and capacity checks use the exact total.
