---
"accounts-api": patch
"ownsand-api": patch
---

The UUID v7 generator now commits its state only after the entropy read and the timestamp range check both succeed. Before, a clock reading beyond the 48-bit timestamp limit left the generator at an out-of-range timestamp, so every later call failed with "timestamp out of range" even after the clock returned to a valid time. The generator also reads entropy inside its lock now, so a random reader that is not safe for concurrent use no longer sees overlapping calls.
