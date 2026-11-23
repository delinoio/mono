---
"ownsand-api": patch
---

Reject an `R2_ENDPOINT` that has a path other than an empty path or a single `/` when the server loads its configuration. The error names `R2_ENDPOINT` and does not include the value. An existing deployment that sets a path, such as `https://<account>.r2.cloudflarestorage.com/storage`, now fails to start until the path is removed from `R2_ENDPOINT`.
