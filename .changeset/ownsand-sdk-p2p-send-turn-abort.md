---
"ownsand-sdk": patch
---

Return the abort reason of a peer-to-peer request as soon as its caller cancels it while its frame waits behind another request's frame for the send turn, instead of waiting until the active sender releases the turn. The canceled frame still sends nothing, and the active frame and the session are unaffected.
