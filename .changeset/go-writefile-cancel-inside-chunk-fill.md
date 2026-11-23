---
"ownsand-sdk": patch
---

The Go `WriteFile` now checks the caller's context before every call to the source's `Read`, including the further `Read` calls that fill one chunk after a short read. A read that returns a partial chunk and cancels the context no longer lets the SDK read the source again; `WriteFile` returns the context error with the last acknowledged byte count and sends no further write request.
