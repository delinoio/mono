---
"ownsand-api": patch
---

Reject new Windows guest sandboxes during placement when the request selects `OUTBOUND` or omits the network mode. Set the network mode to `NONE` for Windows guests.
