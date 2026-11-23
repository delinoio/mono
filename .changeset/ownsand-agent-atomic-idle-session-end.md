---
"ownsand-agent": patch
---

A peer session now ends as idle only when no request is open and no frame arrived for the idle time at the moment the session ends. Before, the agent checked the idle condition, released its lock and ended the session in a second step, so a request that arrived in between was cut off with an `IDLE` session end. Activity before the transition now keeps the session open, and a request that arrives after the transition cannot register or reach the dispatcher.
