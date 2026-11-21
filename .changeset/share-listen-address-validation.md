---
"ownsand-api": patch
---

Reject an `ADDR` or `STUN_ADDR` value whose host is not empty, an IP address or an RFC 1123 hostname, such as `not a host:80`, at startup. `ownsand-api` now validates listen addresses with the same rule as `accounts-api`.
