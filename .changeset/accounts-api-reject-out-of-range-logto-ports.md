---
"accounts-api": patch
---

Reject a `LOGTO_ISSUER` or `LOGTO_MANAGEMENT_ENDPOINT` URL with a port above 65535, including a port that has too many digits, when the server starts. The server now exits with an error that names the setting and does not contain the URL, instead of failing on the first request to Logto.
