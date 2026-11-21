---
"ownsand-api": patch
---

Reject CORS_ORIGINS entries that contain a fragment at startup, because browsers never send one in the Origin header.
