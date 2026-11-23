---
"ownsand-sdk": patch
---

A peer-to-peer channel failure that an operation of an old session reports late no longer closes the healthy replacement session that another operation already opened. The operation of the old session still gets its failure (an unavailable data path error on a forced `PEER_TO_PEER` transport, relay fallback on an automatic handle); a failure of the installed session still retires it.
