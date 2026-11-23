---
"accounts-api": patch
"ownsand-api": patch
---

Store an IPv4 host of each `CORS_ORIGINS` and `WEB_ORIGIN` entry in dotted decimal, as a browser serializes it in the `Origin` header, so `http://127.1:3000` matches `http://127.0.0.1:3000`. A host whose last label is a number but that is not a valid IPv4 address, such as `1.2.3.4.5`, now stops the server at startup.
