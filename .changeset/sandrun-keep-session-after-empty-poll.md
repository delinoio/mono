---
"sandrun-cli": patch
---

Keep the last known dev session when a successful wait poll omits the session. If the wait reaches its deadline, the MCP tool can still report the last known state.
