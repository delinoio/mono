---
"ownsand-sdk": patch
---

The peer-to-peer session setup now registers its first waiter before it calls the access token callback. A callback that aborts the caller or closes the handle synchronously now ends the operation at once with the abort reason or the closed error, even if its token promise never settles. Before, that operation stayed pending.
