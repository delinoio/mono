---
"ownsand-api": patch
---

Reject a `STUN_RATE_LIMIT_BURST_BYTES` below 64 bytes at startup. 64 bytes is the smallest burst that still admits the IPv4 (40-byte) and IPv6 (52-byte) binding success responses and the smallest 64-byte STUN 420 error response. A larger 420 response, for a request with more unknown attributes, can still exceed the burst, and the rate limiter drops it.
