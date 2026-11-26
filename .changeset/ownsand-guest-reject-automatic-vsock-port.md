---
"ownsand-agent": patch
---

The OwnSand guest helper rejects the automatic vsock port (`4294967295`) in its main listener and Linux Hyper-V tunnel configuration. Both configurations require a fixed port. Use a guest image built with the updated helper to get the fix.
