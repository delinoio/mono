---
"ownsand-sdk": patch
---

`writeFile` now reads a readable stream from another realm (for example one with a `node:vm` prototype) or with a replaced prototype through a stream reader. Before, such a stream did not match `instanceof ReadableStream` and was read as a generic async iterable, so an abort during a pending read left the stream locked and did not cancel its source. Now the abort rejects with the caller's reason, cancels the source with that reason and releases the lock, even when the source never yields or its cancel never settles.
