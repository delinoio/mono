---
"ownsand-api": patch
---

Keep a `FAILED` snapshot record when a state report omits it. A snapshot whose create failed (never reported by the agent, or ended by the create-enqueue compensation) stays `FAILED` and readable through `GetSnapshot` until `DeleteSnapshot` removes it, instead of disappearing at the second report that omits it.
