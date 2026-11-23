---
"ownsand-api": patch
---

A background retry of a pending unlink now reads the clock after it obtains the organization lock. A retry that waited for the lock while the deadline passed sends the call at once, where before it compared the deadline with a clock reading from before the wait and left the row for the next round. A retry whose row another transaction removed or moved into the future during the wait still sends nothing.
