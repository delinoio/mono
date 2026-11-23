---
"ownsand-api": patch
---

Close the STUN UDP socket and stop its helper goroutines when the STUN worker returns after a read error, instead of keeping them until the process exits.
