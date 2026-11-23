---
"@delino/ownsand-protos": patch
---

Add the additive `int64` fields `DeviceAllocation.vcpu_total_count` (tag 5, with explicit presence) and `Device.effective_vcpu_limit_count` (tag 16) to the generated TypeScript bindings. They carry the exact allocated vCPUs and the exact effective vCPU limit; the `int32` fields `vcpu_count` and `effective_vcpu_limit` stay, capped at 2147483647.
