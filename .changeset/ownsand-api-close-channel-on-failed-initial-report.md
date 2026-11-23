---
"ownsand-api": patch
---

Close an agent channel whose first full state report fails during database reconciliation, so that the agent reconnects and reports again instead of leaving the device unplaceable behind heartbeats.
