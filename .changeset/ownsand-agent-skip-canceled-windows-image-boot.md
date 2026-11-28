---
"ownsand-agent": patch
---

Skip the first Hyper-V boot when a Windows image build is canceled or its context expires during offline preparation. The command returns the context error, keeps the staged build for inspection, and preserves the installed image.
