---
"ownsand-sdk": patch
---

Keep the UUID v7 generator state unchanged when `UuidV7Generator.next()` throws. Before, a clock reading outside the 48-bit timestamp range left the out-of-range timestamp in the generator, so every later call on the same instance failed even after the clock recovered. The generator now also rejects a non-finite clock reading with a `RangeError` instead of encoding it as timestamp 0.
