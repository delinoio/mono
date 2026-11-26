---
"sandrun-api": patch
---

Make SandRun API lease waits require current ownership. After expiry, wait for a fresh acquisition or return the context error.
