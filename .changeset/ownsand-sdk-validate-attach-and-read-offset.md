---
"ownsand-sdk": patch
---

Reject a negative `readFile` offset, a negative `attachExec` offset and an empty, malformed or uppercase `attachExec` exec ID before any request is sent.
