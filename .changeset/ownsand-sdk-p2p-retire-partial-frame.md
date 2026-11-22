---
"ownsand-sdk": patch
---

Retire a peer-to-peer session when a data channel send fails after the first message of a frame went out, so no later frame follows the partial one on the same byte stream. Calls that wait on the retired session see it ended and recover as they do for any other peer failure; a cancellation before the first message still keeps the session.
