---
"ownsand-sdk": patch
---

The OwnSand Go SDK's `ReadFileFrom` preserves the requested nonnegative offset when local request validation fails. A rejected read returns the validation error without sending a request or calling the writer.
