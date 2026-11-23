---
"ownsand-api": patch
---

Treat a successful Logto token response with a malformed body, for example `expires_in` as a string, as a token request failure instead of caching a token with a fallback lifetime. The error carries no response content.
