---
"ownsand-sdk": patch
"ownsand-cli": patch
"sandrun-api": patch
---

Reject a finished OwnSand write when the reported file size differs from the requested total. The SDK returns a data path error on either transport without retrying.
