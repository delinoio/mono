---
"accounts-api": patch
"ownsand-api": patch
"sandrun-api": patch
---

Reject Unicode whitespace in decoded hostnames in `CORS_ORIGINS` and in Accounts API's `WEB_ORIGIN`. Correct affected settings before upgrading. Use Punycode for internationalized hostnames.
