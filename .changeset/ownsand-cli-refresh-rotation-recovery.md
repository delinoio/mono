---
"ownsand-cli": patch
---

Use a fresh token saved by another OwnSand CLI process even after the last refresh attempt fails. Check for a saved token again when the wait for a concurrent refresh ends.
