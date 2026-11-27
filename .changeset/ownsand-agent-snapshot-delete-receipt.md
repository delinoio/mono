---
"ownsand-agent": patch
---

Apply snapshot deletes in command receipt order. An earlier delete can no longer cancel a later snapshot create or remove the copy it publishes. Disk cleanup runs in the background.
