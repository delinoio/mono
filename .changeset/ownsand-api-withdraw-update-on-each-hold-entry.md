---
"ownsand-api": patch
---

The control plane now sends a withdrawal of a queued agent update on each entry into the HOLD policy. Before, a HOLD that followed a FOLLOW_DEFAULT period without a newer target sent no withdrawal, because the earlier withdrawal marker stayed on the channel. A repeated evaluation inside one HOLD period still sends one withdrawal.
