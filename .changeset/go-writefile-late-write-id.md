---
"ownsand-sdk": patch
---

The Go `WriteFile` now generates the `write_id`, the binding salt and the relay grant digest after it has read the first chunk and the lookahead, right before the first send, so a slow source no longer ages the UUID v7 timestamp past the 15-minute operation ID admission window. A path or mode error is still returned before it reads the source.
