---
"ownsand-sdk": patch
---

The peer-to-peer data channel now accepts binary messages from another JavaScript realm. A cross-realm `ArrayBuffer` or `Uint8Array` (a view supplies only its own bytes) reaches the frame decoder, where before it ended the session as a non-binary message. Strings and `DataView` messages still end the session.
