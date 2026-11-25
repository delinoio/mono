---
"ownsand-agent": patch
---

Fix OwnSand guest file reads on Linux when a sysfs attribute reports a different size from the bytes it returns. The guest streams the copied bytes and uses their count as the snapshot size for initial and resumed reads.
