---
"ownsand-agent": patch
---

When an exec response stream ends, the guest helper and device agent stop reading ready events for that response. A caller can still attach from its last received offsets to read the retained output and final result.
