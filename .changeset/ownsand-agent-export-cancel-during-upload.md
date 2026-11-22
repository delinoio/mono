---
"ownsand-agent": patch
---

A snapshot export now returns the context error when the context is cancelled while it reads the response of a part upload or after the final part. Before, the export dropped the read error and returned success for a cancelled upload.
