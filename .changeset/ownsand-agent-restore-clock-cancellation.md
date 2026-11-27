---
"ownsand-agent": patch
---

Fix Firecracker sandbox creation when the create context ends during the guest clock update after a memory restore. The agent returns the context error and runs failed-create cleanup without starting a replacement VM.
