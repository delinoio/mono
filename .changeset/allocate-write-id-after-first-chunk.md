---
"ownsand-sdk": patch
---

`writeFile` now generates the `write_id` when its first chunk is ready to send, not before it reads the source. Before, a source that needed 15 minutes or more for its first sendable chunk made the agent reject the upload with `not_found` because the operation ID had expired.
