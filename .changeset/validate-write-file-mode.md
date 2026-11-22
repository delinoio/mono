---
"ownsand-sdk": patch
---

`writeFile` now rejects a `mode` that is not an integer from 0 to `0o7777` with an `OwnSandValidationError` for `finish.mode` before it sends a request. Before, the agent rejected such a mode with `invalid_argument` after the whole upload.
