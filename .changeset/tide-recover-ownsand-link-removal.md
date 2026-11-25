---
"tide-api": patch
---

Let administrators retry OwnSand unlinking and owners retry organization deletion after an abandoned link removal lease expires. Keep runner dispatch and key replacement blocked until teardown succeeds or the active attempt releases its claim.
