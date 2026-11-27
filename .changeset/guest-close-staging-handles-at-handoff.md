---
"ownsand-agent": patch
---

The guest helper closes all write staging handles before releasing its state directory lock. A replacement helper in the same process can then remove expired upload files and their recovery records on Windows.
