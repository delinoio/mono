---
"ownsand-api": patch
---

Revalidate the billing status of a queued sandbox pause or resume before it acts. A recovery that commits before a queued pause reaches its slot no longer marks or pauses the sandbox, and a new suspension that commits before a queued resume no longer resumes it.
