---
"ownsand-console": patch
---

The create sandbox form now shows a Labels field error when a label key or value contains a U+0000 character, and sends no request until it is removed. The control plane rejects such a label, so the form no longer sends it.
