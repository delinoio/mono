---
"ownsand-api": patch
"ownsand-agent": patch
"@delino/ownsand-protos": patch
---

Keep the billing pause mark while an automatic resume may still finish. Return a retryable error for a caller pause until the control plane receives and reconciles completion from the device.
