---
"ownsand-sdk": patch
---

`writeFile` now recognizes a `Uint8Array` or an `ArrayBuffer` source from another realm (for example a `node:vm` context) as binary data. Before, it did not match the local constructors and treated the value as a generic iterable.
