---
"tide-cli": patch
---

Keep the Tide MCP `wait_session` lookup within its deadline. Return the last observed state when a positive wait expires, or report `deadline_exceeded` if no state was read. A zero wait makes one bounded lookup.
