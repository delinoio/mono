---
"ownsand-sdk": patch
---

`createSandbox` now rejects a fractional, `NaN`, infinite or out-of-range `vcpuCount`, `memoryMib` or `diskGib` with an `OwnSandValidationError` before it sends a request, and `exec` rejects a `timeoutMs` above the longest protobuf Duration. Before, such input failed late with a `RangeError` or an internal serialization error.
