---
"accounts-api": patch
"ownsand-api": patch
---

Store an IPv6 host of each `CORS_ORIGINS` and `WEB_ORIGIN` entry in the compressed lower-case form that a browser serializes in the `Origin` header, so `https://[0:0:0:0:0:0:0:1]:443` is stored as `https://[::1]` and matches the preflight from `https://[::1]`. An IPv6 address with a zone, which browsers reject, now stops the server at startup.
