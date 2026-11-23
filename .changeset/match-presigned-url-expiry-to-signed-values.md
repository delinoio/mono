---
"ownsand-api": patch
---

The expiry returned with a signed part URL now equals the signing time plus the lifetime encoded in the URL. When the signing clock had a fractional second, the returned expiry could be up to one second later than the expiry the storage service enforced.
