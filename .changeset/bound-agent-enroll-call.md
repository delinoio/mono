---
"ownsand-agent": patch
---

`ownsand-agent enroll` now ends the unary Enroll call after 30 seconds when the control plane accepts it and never answers, prints an enrollment timeout error and exits with code 1. An earlier caller deadline or cancellation still ends the call at once. The pending device key stays on disk, so a retry sends the same public key.
