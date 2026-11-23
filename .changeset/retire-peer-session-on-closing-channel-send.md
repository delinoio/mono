---
"ownsand-sdk": patch
---

A first message that fails to send on a closing or closed peer-to-peer data channel now retires the session as a channel failure, so an automatic handle falls back to the relay before the close event arrives. Before, the raw send error reached the caller and the dead session stayed installed. A first-message failure on a channel that is still open keeps the session as before.
