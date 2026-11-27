---
"ownsand-agent": patch
---

Send `started` before `paused` when attaching to an exec that the agent has admitted and the guest has yet to start. The first response omits the start time. Later attachments replay the guest timestamp once it is available.
