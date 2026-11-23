---
"ownsand-api": patch
---

Reject a blank or delimiter-only `STUN_URLS` list at startup. The server no longer advertises an empty STUN server list to agents and `GetIceServers` callers.
