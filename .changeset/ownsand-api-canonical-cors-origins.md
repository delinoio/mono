---
"ownsand-api": patch
---

Store each `CORS_ORIGINS` entry in the form that a browser sends in the `Origin` header (lower-case host, no default port), so an entry such as `HTTPS://Console.Example.ORG:443` matches `https://console.example.org`.
