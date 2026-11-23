---
"ownsand-console": patch
---

A device removal, organization deletion or self-removal that succeeds after its confirmation dialog was dismissed no longer redirects the console or adds a history entry. The redirect now happens only while the confirmation that sent the request is still open, and the cache refresh still runs.
