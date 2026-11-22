---
"ownsand-console": patch
---

Keep the stored memory and disk limits of a device exact when another limit is saved. The device page now edits the memory and disk limits as whole numbers of the full `int64` range and rejects a negative, decimal or out-of-range value with a field error.
