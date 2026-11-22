---
"ownsand-sdk": patch
---

Release the activity registration and the caller-signal listener of a relay stream when the caller cancels the call while the consumer has abandoned its iteration, instead of keeping them until the suspended generator is resumed.
