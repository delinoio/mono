---
"ownsand-agent": patch
---

Protect and verify the Windows agent state directory before first use, including when service configuration is invalid. Reject reparse points at fixed state paths and check the owners of deeper links without following them.
