---
"ownsand-agent": patch
---

Keep the guest helper's state directory locked until admitted file writes and write sweeps finish during shutdown. A replacement helper can open the directory once those operations finish.
