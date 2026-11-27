---
"ownsand-sdk": patch
---

Switch automatic sandbox handles to relay when a peer data channel fails between operations. Later operations on the same handle skip peer setup. A normal session end and forced peer-to-peer mode still allow a new peer session.
