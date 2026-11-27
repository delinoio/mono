---
"ownsand-cli": patch
"sandrun-cli": patch
---

On Windows, coordinate credential reads with saves and sign-outs so concurrent CLI readers cannot block file replacement or removal.
