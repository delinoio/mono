---
"ownsand-api": patch
---

Report a server-side deadline that passes while a relay call waits for the device's relay connection or for its stream to open as `deadline_exceeded`, and a cancellation as `canceled`, instead of `unavailable`. A missing or closed relay connection, and the expiry of the 10-second connection wait while the call is still active, stay `unavailable`.
