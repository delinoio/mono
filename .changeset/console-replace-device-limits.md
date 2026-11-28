---
"ownsand-console": patch
"ownsand-api": patch
"ownsand-cli": patch
"@delino/ownsand-protos": patch
---

The OwnSand API adds the strict `ReplaceDeviceLimits` RPC. It replaces all four limits together, requires all four fields, and preserves an explicit zero for vCPU, memory, or disk.

The `ownsand device set-limits` command and the console's device settings use this RPC to save limits. The console reports missing limits before sending a request. The generated TypeScript bindings expose `ReplaceDeviceLimits` on `DeviceService`.
