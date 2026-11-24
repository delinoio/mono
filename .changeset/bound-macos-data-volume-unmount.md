---
"ownsand-agent": patch
---

Limit the macOS image build's Data volume unmount to 30 seconds so whole-disk detach can proceed if unmount stalls. Preserve any earlier provisioning error.
