---
"tide-cli": patch
---

Enforce one 50-second deadline for each Tide MCP profile list and session management call. The deadline includes all pages and the profile lookup before session creation. Earlier caller deadlines and cancellation still apply.
