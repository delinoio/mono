---
"accounts-api": patch
---

Reject a `LOGTO_ISSUER`, `LOGTO_MANAGEMENT_ENDPOINT`, `CORS_ORIGINS` or `WEB_ORIGIN` value that has a port but no host name, such as `https://:443`, at startup.
