---
"ownsand-console": patch
---

Make the mock API reject `TransferOwnership` to the caller with `InvalidArgument`, after the owner check and before any change, so the billing link is no longer removed by a transfer that changes no owner.
