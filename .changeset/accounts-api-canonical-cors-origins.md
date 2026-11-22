---
"accounts-api": patch
---

Store each `CORS_ORIGINS` and `WEB_ORIGIN` entry in the form that a browser sends in the `Origin` header (lower-case host, no default port), so an entry such as `HTTPS://Accounts.Delino.IO:443` matches `https://accounts.delino.io`.
